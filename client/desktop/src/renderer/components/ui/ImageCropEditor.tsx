import React, { useState, useEffect, useRef, useCallback } from 'react';
import Modal from './Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { apiFetch } from '../../services/apiClient';
import './ImageCropEditor.css';

/** Configuration for uploading the cropped image to object storage. */
export interface CropUploadConfig {
  /** The API endpoint to POST the cropped file to, e.g. '/api/v1/media/upload/avatar' */
  endpoint: string;
  /** Additional form fields to include (e.g. { server_id: '...' }) */
  extraFields?: Record<string, string>;
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

  // Upload a cropped blob to object storage, return the proxy URL
  const uploadBlob = useCallback(async (blob: Blob, cfg: CropUploadConfig): Promise<string> => {
    const formData = new FormData();
    const ext = blob.type === 'image/png' ? 'png' : 'jpg';
    formData.append('file', blob, `cropped.${ext}`);

    if (cfg.extraFields) {
      for (const [key, value] of Object.entries(cfg.extraFields)) {
        formData.append(key, value);
      }
    }

    const response = await apiFetch(cfg.endpoint, {
      method: 'POST',
      body: formData,
    });

    if (!response.ok) {
      const data = await response.json().catch(() => ({ error: 'Upload failed' }));
      throw new Error(data.error || 'Failed to upload image');
    }

    const data = await response.json();
    if (!data || typeof data.url !== 'string' || data.url.length === 0) {
      throw new Error('Upload succeeded but response did not include image URL');
    }
    return data.url;
  }, []);

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
      const url = upload ? await uploadBlob(blob, upload) : await blobToDataUrl(blob);
      onConfirm(url);
    } catch (error) {
      setUploadError(error instanceof Error ? error.message : 'Failed to upload image');
    } finally {
      setIsUploading(false);
    }
  }, [getCroppedBlob, upload, uploadBlob, blobToDataUrl, onConfirm]);

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
    </Modal>
  );
};

export default ImageCropEditor;
