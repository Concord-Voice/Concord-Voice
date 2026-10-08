import React, { useState, useEffect, useRef, useCallback } from 'react';
import Modal from './Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import DangerousActionStepUpDialog, {
  type DangerousActionSendResult,
} from '../Auth/DangerousActionStepUpDialog';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import {
  describeFailureWith,
  FIRST_SEND_SESSION_CHANGED,
} from '../../services/system/dangerousActionRequest';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { serverErrorText } from '../../services/system/stepUpRouteAdapters';
import { stepUpSeed } from '../../services/system/stepUpSeed';
import './ImageCropEditor.css';

/** The server image uploads the control plane gates (#3454): one purpose per route. */
type ServerImagePurpose = Extract<
  StepUpPurpose,
  'media.server_icon_upload' | 'media.server_banner_upload'
>;

/** Configuration for uploading the cropped image to object storage. */
export interface CropUploadConfig {
  /** The API endpoint to POST the cropped file to, e.g. '/api/v1/media/upload/avatar' */
  endpoint: string;
  /** Additional form fields to include (e.g. { server_id: '...' }) */
  extraFields?: Record<string, string>;
  /**
   * Set for an upload the server may ask to verify (#3456). A refusal then opens the
   * verification dialog over this editor, and the same crop is sent again with `mfa_code`.
   * Omitted (the avatar), nothing about the upload changes.
   */
  stepUpPurpose?: ServerImagePurpose;
  /**
   * Opens verification setup for the dialog's enrolment state, given the function that closes
   * the dialog. Only the page that owns the surrounding form knows what leaving would discard.
   */
  onSetUpVerification?: (closeHost: () => void) => void;
}

/** Where an upload went and what rode with it: what the re-send repeats, not the live prop. */
interface FrozenUploadTarget {
  readonly endpoint: string;
  readonly extraFields?: Readonly<Record<string, string>>;
}

/**
 * The crop that was refused, kept as the first send made it for the re-send: the image, the
 * route and fields it went to, and the account and server it went out as.
 */
interface PendingUpload {
  blob: Blob;
  refusal: StepUpFactorRefusal;
  target: FrozenUploadTarget;
  context: ApiRequestContext;
}

type UploadOutcome =
  | { kind: 'uploaded'; url: string }
  | {
      kind: 'stepUp';
      refusal: StepUpFactorRefusal;
      target: FrozenUploadTarget;
      context: ApiRequestContext;
    };

const UPLOAD_FAILED = 'Failed to upload image';
const NO_URL_MESSAGE = 'Upload succeeded but response did not include image URL';
const describeUploadFailure = describeFailureWith(UPLOAD_FAILED);

const IMAGE_NOUN: Record<ServerImagePurpose, string> = {
  'media.server_icon_upload': 'icon',
  'media.server_banner_upload': 'banner',
};

/** The multipart body for one upload of `blob`. `mfaCode` is only on the re-send. */
function uploadForm(blob: Blob, cfg: FrozenUploadTarget, mfaCode?: string): FormData {
  const formData = new FormData();
  const ext = blob.type === 'image/png' ? 'png' : 'jpg';
  formData.append('file', blob, `cropped.${ext}`);

  if (cfg.extraFields) {
    for (const [key, value] of Object.entries(cfg.extraFields)) {
      formData.append(key, value);
    }
  }
  // An empty code reads to the server as a supplied, wrong one, and is charged as such.
  if (mfaCode) formData.append('mfa_code', mfaCode);
  return formData;
}

/** A response's JSON body, or null when it has none (a proxy's HTML page, an empty body). */
async function readUploadBody(response: Response): Promise<unknown> {
  return response.json().catch(() => null);
}

/**
 * The stored image's URL from a successful upload, or null when the body names none. A 2xx
 * whose body does not parse names none either: the server answered, so it is never a transport
 * failure that "may have acted".
 */
async function uploadedUrl(response: Response): Promise<string | null> {
  const data = await readUploadBody(response);
  if (typeof data !== 'object' || data === null || !('url' in data)) return null;
  const { url } = data;
  return typeof url === 'string' && url.length > 0 ? url : null;
}

/** What re-sending a refused crop came to: the dialog's result, or the stored image's URL. */
type ResendOutcome = DangerousActionSendResult | { kind: 'uploaded'; url: string };

/** The refused crop again, to the route it first went to, with the proven code, admitted against `context`. */
async function resendUpload(
  pending: PendingUpload,
  mfaCode: string | undefined,
  context: ApiRequestContext
): Promise<ResendOutcome> {
  const { blob, target } = pending;
  const response = await apiFetchInContext(
    target.endpoint,
    { method: 'POST', body: uploadForm(blob, target, mfaCode) },
    context
  );
  if (!response.ok) {
    return { kind: 'refused', status: response.status, body: await readUploadBody(response) };
  }
  const url = await uploadedUrl(response);
  // A 2xx that stored nothing is a failure shown in the dialog, never an image to apply.
  if (url === null) {
    return { kind: 'refused', status: response.status, body: { error: NO_URL_MESSAGE } };
  }
  return { kind: 'uploaded', url };
}

export interface ImageCropEditorProps {
  isOpen: boolean;
  onClose: () => void;
  /** Called with the resulting URL (proxy path if upload is configured, data URL otherwise) */
  onConfirm: (url: string) => void;
  imageFile: File | null;
  title: string;
  cropShape: { type: 'circle' | 'rectangle' };
  output: { width: number; height: number; quality: number };
  /** When provided, the cropped image is uploaded to object storage via this config.
   *  The onConfirm callback receives the proxy URL from the server response.
   *  When omitted, onConfirm receives a base64 data URL (legacy fallback). */
  upload?: CropUploadConfig;
}

// Padding around the crop area within the canvas (px at display scale)
const CANVAS_PADDING = 40;
// Semi-transparent overlay color
const OVERLAY_COLOR = 'rgba(0, 0, 0, 0.55)';
// Crop border color
const BORDER_COLOR = 'rgba(255, 255, 255, 0.4)';

const ImageCropEditor: React.FC<ImageCropEditorProps> = ({
  isOpen,
  onClose,
  onConfirm,
  imageFile,
  title,
  cropShape,
  output,
  upload,
}) => {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const animFrameRef = useRef(0);
  const imageRef = useRef<HTMLImageElement | null>(null);

  const [zoom, setZoom] = useState(1);
  const [offset, setOffset] = useState({ x: 0, y: 0 });
  const [minZoom, setMinZoom] = useState(1);
  const [isLoading, setIsLoading] = useState(true);
  const [isUploading, setIsUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [isSmallImage, setIsSmallImage] = useState(false);
  const [stepUp, setStepUp] = useState<PendingUpload | null>(null);
  const applyRef = useRef<HTMLButtonElement>(null);
  const uploadedUrlRef = useRef<string | null>(null);

  // Drag state stored in ref to avoid re-renders during drag
  const dragRef = useRef({
    active: false,
    startX: 0,
    startY: 0,
    startOffsetX: 0,
    startOffsetY: 0,
  });

  // Canvas display dimensions
  const DISPLAY_WIDTH = 552; // modal-large (600px) minus body padding (48px)
  const cropAspect = output.width / output.height;
  const cropDisplayWidth = Math.min(DISPLAY_WIDTH - CANVAS_PADDING * 2, DISPLAY_WIDTH);
  const cropDisplayHeight = cropDisplayWidth / cropAspect;
  const canvasDisplayHeight = cropDisplayHeight + CANVAS_PADDING * 2;

  // Circle crops require square output (1:1 aspect ratio)
  if (cropShape.type === 'circle' && output.width !== output.height) {
    throw new Error(
      'ImageCropEditor: circle crop requires square output (width must equal height)'
    );
  }

  // Clamp offset so image always covers the crop area
  const clampOffset = useCallback(
    (ox: number, oy: number, z: number): { x: number; y: number } => {
      const img = imageRef.current;
      if (!img) return { x: 0, y: 0 };

      // Image display dimensions at this zoom
      const imgDisplayW = img.naturalWidth * z;
      const imgDisplayH = img.naturalHeight * z;

      const maxOx = Math.max(0, (imgDisplayW - cropDisplayWidth) / 2);
      const maxOy = Math.max(0, (imgDisplayH - cropDisplayHeight) / 2);

      return {
        x: Math.max(-maxOx, Math.min(maxOx, ox)),
        y: Math.max(-maxOy, Math.min(maxOy, oy)),
      };
    },
    [cropDisplayWidth, cropDisplayHeight]
  );

  // Load image when file changes
  useEffect(() => {
    if (!isOpen || !imageFile) return;

    // Synchronous clean-slate reset before the async image load below computes
    // and applies the real geometry in img.onload. None of these setters are in
    // this effect's dependency array, so they cannot re-trigger the effect (no
    // render loop) — this is a prop-driven external-data reset, not a smell.
    /* eslint-disable @eslint-react/set-state-in-effect -- intentional: see comment above */
    setIsLoading(true);
    setIsUploading(false);
    setUploadError(null);
    setZoom(1);
    setOffset({ x: 0, y: 0 });
    setIsSmallImage(false);
    setStepUp(null);
    /* eslint-enable @eslint-react/set-state-in-effect -- re-enable after the clean-slate reset block above */

    const url = URL.createObjectURL(imageFile);

    const img = new Image();
    img.onload = () => {
      imageRef.current = img;

      // Calculate min zoom so the image fully covers the crop area
      const scaleX = cropDisplayWidth / img.naturalWidth;
      const scaleY = cropDisplayHeight / img.naturalHeight;
      const min = Math.max(scaleX, scaleY);

      setMinZoom(min);
      setZoom(min);
      setOffset({ x: 0, y: 0 });
      setIsSmallImage(img.naturalWidth < output.width || img.naturalHeight < output.height);
      setIsLoading(false);
    };
    img.onerror = () => {
      setIsLoading(false);
      setUploadError('Failed to load image. Please try a different file.');
    };
    img.src = url;

    return () => {
      URL.revokeObjectURL(url);
      imageRef.current = null;
    };
  }, [isOpen, imageFile, cropDisplayWidth, cropDisplayHeight, output.width, output.height]);

  // Size the canvas once when image loads or dimensions change (avoids resetting on every drag/zoom)
  useEffect(() => {
    if (isLoading || !imageRef.current) return;
    const canvas = canvasRef.current;
    if (!canvas) return;

    const dpr = globalThis.devicePixelRatio || 1;
    canvas.width = DISPLAY_WIDTH * dpr;
    canvas.height = canvasDisplayHeight * dpr;
    canvas.style.width = `${DISPLAY_WIDTH}px`;
    canvas.style.height = `${canvasDisplayHeight}px`;
  }, [isLoading, canvasDisplayHeight, DISPLAY_WIDTH]);

  // Render preview canvas (runs on every zoom/offset change — lightweight, no resize)
  useEffect(() => {
    if (isLoading || !imageRef.current) return;

    const canvas = canvasRef.current;
    if (!canvas) return;

    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    const dpr = globalThis.devicePixelRatio || 1;
    const w = DISPLAY_WIDTH;
    const h = canvasDisplayHeight;

    const render = () => {
      const img = imageRef.current;
      if (!img) return;

      // Reset transform and apply HiDPI scaling
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);

      // Crop area position (centered in canvas)
      const cropX = (w - cropDisplayWidth) / 2;
      const cropY = (h - cropDisplayHeight) / 2;

      // Image draw position (centered + offset)
      const imgW = img.naturalWidth * zoom;
      const imgH = img.naturalHeight * zoom;
      const imgX = cropX + cropDisplayWidth / 2 - imgW / 2 + offset.x;
      const imgY = cropY + cropDisplayHeight / 2 - imgH / 2 + offset.y;

      ctx.save();
      ctx.drawImage(img, imgX, imgY, imgW, imgH);
      ctx.restore();

      // Draw overlay with crop cutout
      ctx.save();
      ctx.fillStyle = OVERLAY_COLOR;

      if (cropShape.type === 'circle') {
        ctx.fillRect(0, 0, w, h);
        ctx.globalCompositeOperation = 'destination-out';
        ctx.beginPath();
        const radius = cropDisplayWidth / 2;
        ctx.arc(cropX + radius, cropY + radius, radius, 0, Math.PI * 2);
        ctx.fill();
        ctx.globalCompositeOperation = 'source-over';

        ctx.strokeStyle = BORDER_COLOR;
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.arc(cropX + radius, cropY + radius, radius, 0, Math.PI * 2);
        ctx.stroke();
      } else {
        ctx.fillRect(0, 0, w, cropY);
        ctx.fillRect(0, cropY + cropDisplayHeight, w, h - cropY - cropDisplayHeight);
        ctx.fillRect(0, cropY, cropX, cropDisplayHeight);
        ctx.fillRect(
          cropX + cropDisplayWidth,
          cropY,
          w - cropX - cropDisplayWidth,
          cropDisplayHeight
        );

        ctx.strokeStyle = BORDER_COLOR;
        ctx.lineWidth = 1;
        ctx.strokeRect(cropX, cropY, cropDisplayWidth, cropDisplayHeight);
      }

      ctx.restore();
    };

    animFrameRef.current = requestAnimationFrame(render);
    return () => cancelAnimationFrame(animFrameRef.current);
  }, [
    isLoading,
    zoom,
    offset,
    cropShape.type,
    cropDisplayWidth,
    cropDisplayHeight,
    canvasDisplayHeight,
    DISPLAY_WIDTH,
  ]);

  // Mouse wheel zoom
  const handleWheel = useCallback(
    (e: React.WheelEvent) => {
      e.preventDefault();
      const maxZoom = minZoom * 5;
      const step = 0.02 * (minZoom * 5); // Scale step to zoom range

      setZoom((prev) => {
        const next = Math.max(minZoom, Math.min(maxZoom, prev - e.deltaY * step * 0.01));
        // Clamp offset at new zoom
        setOffset((o) => clampOffset(o.x, o.y, next));
        return next;
      });
    },
    [minZoom, clampOffset]
  );

  // Drag handlers
  const handleMouseDown = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      dragRef.current = {
        active: true,
        startX: e.clientX,
        startY: e.clientY,
        startOffsetX: offset.x,
        startOffsetY: offset.y,
      };
    },
    [offset]
  );

  const handleMouseMove = useCallback(
    (e: React.MouseEvent) => {
      if (!dragRef.current.active) return;

      const dx = e.clientX - dragRef.current.startX;
      const dy = e.clientY - dragRef.current.startY;

      const newOffset = clampOffset(
        dragRef.current.startOffsetX + dx,
        dragRef.current.startOffsetY + dy,
        zoom
      );
      setOffset(newOffset);
    },
    [zoom, clampOffset]
  );

  const handleMouseUp = useCallback(() => {
    dragRef.current.active = false;
  }, []);

  // Zoom slider
  const handleZoomSlider = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const next = Number.parseFloat(e.target.value);
      setZoom(next);
      setOffset((o) => clampOffset(o.x, o.y, next));
    },
    [clampOffset]
  );

  // Render the cropped image to an offscreen canvas and return as Blob
  const getCroppedBlob = useCallback((): Promise<Blob | null> => {
    return new Promise((resolve) => {
      const img = imageRef.current;
      if (!img) {
        resolve(null);
        return;
      }

      const outCanvas = document.createElement('canvas');
      outCanvas.width = output.width;
      outCanvas.height = output.height;
      const ctx = outCanvas.getContext('2d');
      if (!ctx) {
        resolve(null);
        return;
      }

      // Calculate source rectangle in original image coordinates
      const imgDisplayW = img.naturalWidth * zoom;
      const imgDisplayH = img.naturalHeight * zoom;

      const srcDisplayX = imgDisplayW / 2 - offset.x - cropDisplayWidth / 2;
      const srcDisplayY = imgDisplayH / 2 - offset.y - cropDisplayHeight / 2;

      const sx = srcDisplayX / zoom;
      const sy = srcDisplayY / zoom;
      const sw = cropDisplayWidth / zoom;
      const sh = cropDisplayHeight / zoom;

      ctx.drawImage(img, sx, sy, sw, sh, 0, 0, output.width, output.height);

      // Use PNG for circle crops (preserve transparency), JPEG for banners
      const mimeType = cropShape.type === 'circle' ? 'image/png' : 'image/jpeg';
      outCanvas.toBlob((blob) => resolve(blob), mimeType, output.quality);
    });
  }, [zoom, offset, cropDisplayWidth, cropDisplayHeight, output, cropShape.type]);

  // Upload a cropped blob to object storage: the proxy URL, or the refusal that asks to verify.
  // The route and fields are frozen here, with the account and server the send went out as.
  const uploadBlob = useCallback(
    async (blob: Blob, cfg: CropUploadConfig): Promise<UploadOutcome> => {
      const target: FrozenUploadTarget = {
        endpoint: cfg.endpoint,
        extraFields: cfg.extraFields === undefined ? undefined : { ...cfg.extraFields },
      };
      const context = captureApiRequestContext();
      const response = await apiFetchInContext(
        target.endpoint,
        { method: 'POST', body: uploadForm(blob, target) },
        context
      );

      if (!response.ok) {
        const data = await readUploadBody(response);
        const refusal = cfg.stepUpPurpose === undefined ? null : stepUpSeed(response.status, data);
        if (refusal === null) throw new Error(serverErrorText(data) ?? UPLOAD_FAILED);
        // A refusal for an account or server no longer current belongs to the old one.
        if (!apiRequestContextIsCurrent(context)) throw new Error(FIRST_SEND_SESSION_CHANGED);
        return { kind: 'stepUp', refusal, target, context };
      }

      const url = await uploadedUrl(response);
      if (url === null) throw new Error(NO_URL_MESSAGE);
      return { kind: 'uploaded', url };
    },
    []
  );

  // Convert a blob to a data URL (legacy fallback)
  const blobToDataUrl = useCallback((blob: Blob): Promise<string> => {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result as string);
      reader.onerror = reject;
      reader.readAsDataURL(blob);
    });
  }, []);

  // Generate cropped output and optionally upload
  const handleConfirm = useCallback(async () => {
    const blob = await getCroppedBlob();
    if (!blob) return;

    setIsUploading(true);
    setUploadError(null);

    try {
      if (!upload) {
        onConfirm(await blobToDataUrl(blob));
        return;
      }
      const outcome = await uploadBlob(blob, upload);
      if (outcome.kind === 'uploaded') onConfirm(outcome.url);
      else {
        const { refusal, target, context } = outcome;
        setStepUp({ blob, refusal, target, context });
      }
    } catch (error) {
      setUploadError(error instanceof Error ? error.message : UPLOAD_FAILED);
    } finally {
      setIsUploading(false);
    }
  }, [getCroppedBlob, upload, uploadBlob, blobToDataUrl, onConfirm]);

  const closeStepUp = () => setStepUp(null);

  /** The dialog's send. The URL waits in a ref for `onSuccess`, which the dialog fires only if still current. */
  const sendCrop = async (
    mfaCode: string | undefined,
    context: ApiRequestContext
  ): Promise<DangerousActionSendResult> => {
    if (stepUp === null) return { kind: 'aborted' };
    const sent = await resendUpload(stepUp, mfaCode, context);
    if (sent.kind !== 'uploaded') return sent;
    uploadedUrlRef.current = sent.url;
    return { kind: 'ok' };
  };

  const finishStepUp = () => {
    const url = uploadedUrlRef.current;
    uploadedUrlRef.current = null;
    closeStepUp();
    if (url !== null) onConfirm(url);
  };

  const stepUpPurpose = upload?.stepUpPurpose;
  const setUpVerification = upload?.onSetUpVerification;

  if (!isOpen) return null;

  return (
    <Modal isOpen={isOpen} onClose={onClose} title={title} width="large">
      {isLoading ? (
        <div className="image-crop-loading">Loading image...</div>
      ) : (
        <>
          <button
            type="button"
            className="image-crop-preview"
            aria-label="Image crop editor — drag to reposition, scroll to zoom"
            onWheel={handleWheel}
            onMouseDown={handleMouseDown}
            onMouseMove={handleMouseMove}
            onMouseUp={handleMouseUp}
            onMouseLeave={handleMouseUp}
          >
            <canvas ref={canvasRef} className="image-crop-canvas" />
          </button>

          <div className="image-crop-hint">Drag to reposition, scroll to zoom</div>

          {isSmallImage && (
            <div className="image-crop-warning">
              Image is smaller than recommended ({output.width}&times;{output.height}). Quality may
              be reduced.
            </div>
          )}

          {uploadError && <div className="image-crop-error">{uploadError}</div>}

          <div className="image-crop-controls">
            <span className="image-crop-zoom-label">Zoom</span>
            <input
              type="range"
              className="image-crop-zoom-slider"
              min={minZoom}
              max={minZoom * 5}
              step={0.001}
              value={zoom}
              onChange={handleZoomSlider}
              aria-label="Zoom level"
              disabled={isUploading}
            />
          </div>

          <div className="image-crop-actions">
            <button
              type="button"
              className="profile-cancel-btn"
              onClick={onClose}
              disabled={isUploading}
            >
              Cancel
            </button>
            <button
              ref={applyRef}
              type="button"
              className="profile-save-btn"
              onClick={handleConfirm}
              disabled={isUploading || !imageRef.current}
            >
              {isUploading ? (
                <>
                  Uploading...
                  <LoadingSpinner size="small" inline />
                </>
              ) : (
                'Apply'
              )}
            </button>
          </div>
        </>
      )}
      {stepUpPurpose !== undefined && (
        <DangerousActionStepUpDialog
          isOpen={stepUp !== null}
          purpose={stepUpPurpose}
          seed={stepUp?.refusal}
          intro={`This server asks you to verify before you change its ${IMAGE_NOUN[stepUpPurpose]}.`}
          primaryLabel={`Upload ${IMAGE_NOUN[stepUpPurpose]}`}
          busyLabel="Uploading..."
          send={sendCrop}
          capture={stepUp?.context}
          describeFailure={describeUploadFailure}
          onSuccess={finishStepUp}
          onClose={closeStepUp}
          onSetUpVerification={setUpVerification && (() => setUpVerification(closeStepUp))}
          focusFallback={() => applyRef.current}
        />
      )}
    </Modal>
  );
};

export default ImageCropEditor;
