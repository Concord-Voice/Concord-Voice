import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import ImageCropEditor, { type CropUploadConfig } from '@/renderer/components/ui/ImageCropEditor';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  FIXTURE_OTP_2,
  GATED_API_BASE,
  INVALID_CODE,
  MFA_REQUIRED,
  SETUP_LINK,
  stubStepUpRead,
} from '../../../helpers/gatedRoute';
import { stubGatedWrite, writeCodes } from '../../../helpers/gatedWriteRoute';

// The icon and banner upload step-up (#3456 §3.4): a server that enforces MFA refuses the
// code-less multipart upload, and the same crop is sent again with a multipart `mfa_code`. The
// editor, the dialog, the factor hook and the adapter are real; only the network is stubbed.
// "Mutant:" comments name the production change each case turns red.

// jsdom's FormData, Blob and File cannot carry a multipart file part, and undici's
// `request.formData()` asserts on it, so the upload under test needs Node's native classes. Each is
// restored in afterAll so no other suite in the worker sees them.
beforeAll(async () => {
  mswServer.listen({ onUnhandledRequest: 'bypass' });
  const { Blob: NodeBlob, File: NodeFile } = await import('node:buffer');
  vi.stubGlobal('File', NodeFile);
  vi.stubGlobal('Blob', NodeBlob);
  const nodeForm = await new Response('a=b', {
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
  }).formData();
  vi.stubGlobal('FormData', nodeForm.constructor);
});
afterAll(() => {
  mswServer.close();
  vi.unstubAllGlobals();
});
afterEach(() => mswServer.resetHandlers());

const ICON_URL = `${GATED_API_BASE}/api/v1/media/upload/server-icon`;
const BANNER_URL = `${GATED_API_BASE}/api/v1/media/upload/server-banner`;
const AVATAR_URL = `${GATED_API_BASE}/api/v1/media/upload/avatar`;
const STORED_URL = '/api/v1/media/server-icons/server-1';

const iconUpload: CropUploadConfig = {
  endpoint: '/api/v1/media/upload/server-icon',
  extraFields: { server_id: 'server-1' },
  stepUpPurpose: 'media.server_icon_upload',
};
const bannerUpload: CropUploadConfig = {
  endpoint: '/api/v1/media/upload/server-banner',
  extraFields: { server_id: 'server-1' },
  stepUpPurpose: 'media.server_banner_upload',
};
const avatarUpload: CropUploadConfig = { endpoint: '/api/v1/media/upload/avatar' };

const STORED = { status: 200, body: { url: STORED_URL } };

class LoadedImage {
  naturalWidth = 1024;
  naturalHeight = 1024;
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  set src(_: string) {
    setTimeout(() => this.onload?.(), 0);
  }
}

// The component reads `imageFile` through jsdom APIs, so the picked image stays a jsdom File.
const JsdomFile = globalThis.File;

describe('ImageCropEditor upload step-up (#3456)', () => {
  const onClose = vi.fn();
  const onConfirm = vi.fn();
  const file = new File(['pixels'], 'photo.png', { type: 'image/png' });
  const originalImage = globalThis.Image;
  const originalToBlob = HTMLCanvasElement.prototype.toBlob;
  let crops: number;

  /** Every crop is a different blob, so a re-crop would show in the bytes the server receives. */
  function numberTheCrops() {
    crops = 0;
    HTMLCanvasElement.prototype.toBlob = function toBlob(cb: BlobCallback) {
      crops += 1;
      cb(new Blob([`crop-${crops}`], { type: 'image/png' }));
    };
  }

  const renderEditor = (upload: CropUploadConfig, extra: { imageFile?: File } = {}) =>
    render(
      <ImageCropEditor
        isOpen
        onClose={onClose}
        onConfirm={onConfirm}
        imageFile={extra.imageFile ?? file}
        title="Crop Icon"
        cropShape={{ type: 'circle' }}
        output={{ width: 512, height: 512, quality: 0.9 }}
        upload={upload}
      />
    );

  const code = () => screen.findByLabelText(CODE_LABEL);
  const stepUpDialog = () => screen.queryByRole('dialog', { name: DIALOG_TITLE });
  const applyCrop = async () =>
    userEvent.click(await screen.findByRole('button', { name: 'Apply' }));
  const sendCode = async (digits = FIXTURE_OTP, label = 'Upload icon') => {
    await userEvent.type(await code(), digits);
    await userEvent.click(screen.getByRole('button', { name: label }));
  };

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    globalThis.Image = LoadedImage as unknown as typeof Image;
    numberTheCrops();
    stubStepUpRead();
  });

  afterEach(() => {
    globalThis.Image = originalImage;
    HTMLCanvasElement.prototype.toBlob = originalToBlob;
  });

  // Mutant: `uploadBlob` not returning the refusal (the error is thrown into the editor's own
  // banner), or the dialog mounted without waiting for a refusal.
  it('opens the dialog over the editor when the server refuses the code-less upload', async () => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL });
    renderEditor(iconUpload);
    await applyCrop();

    expect(await code()).toBeInTheDocument();
    expect(stepUpDialog()).toHaveAccessibleDescription(
      'This server asks you to verify before you change its icon.'
    );
    expect(screen.getByRole('button', { name: 'Upload icon' })).toBeInTheDocument();
    // The editor stays mounted underneath, with its crop.
    expect(screen.getByRole('dialog', { name: 'Crop Icon', hidden: true })).toBeInTheDocument();
    expect(writes).toHaveLength(1);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  // Mutant: the intro or primary label built from the wrong purpose (icon copy on the banner).
  it('words the banner upload as a banner', async () => {
    stubGatedWrite({ method: 'post', url: BANNER_URL });
    renderEditor(bannerUpload);
    await applyCrop();

    await code();
    expect(stepUpDialog()).toHaveAccessibleDescription(
      'This server asks you to verify before you change its banner.'
    );
    expect(screen.getByRole('button', { name: 'Upload banner' })).toBeInTheDocument();
  });

  // Mutant: the first send carrying `mfa_code` (an empty one is charged as a wrong code), or the
  // multipart shape (file name, extra fields) changed.
  it('sends the first upload exactly as before: a file and the extra fields, no mfa_code', async () => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL });
    renderEditor(iconUpload);
    await applyCrop();
    await code();

    expect(writes[0].fields).toEqual({ server_id: 'server-1' });
    expect(writes[0].file).toEqual({ name: 'cropped.png', type: 'image/png', text: 'crop-1' });
    expect(writes[0].contentType).toMatch(/^multipart\/form-data/);
  });

  // Mutant: the re-send re-crops (a different Blob), drops the extra fields, omits or renames the
  // multipart `mfa_code` field, or sends it in JSON instead.
  it('re-sends the same crop with a multipart mfa_code field, and applies the stored URL once', async () => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL, retries: [STORED] });
    renderEditor(iconUpload);
    await applyCrop();
    await sendCode();

    await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
    expect(onConfirm).toHaveBeenCalledWith(STORED_URL);
    expect(writes).toHaveLength(2);
    expect(writes[1].fields).toEqual({ server_id: 'server-1', mfa_code: FIXTURE_OTP });
    expect(writes[1].file).toEqual(writes[0].file);
    expect(writes[1].json).toBeNull();
    expect(crops).toBe(1);
    expect(stepUpDialog()).not.toBeInTheDocument();
  });

  // Mutant: a refused code treated as success, or the crop dropped after the first re-send.
  it('keeps the dialog and the same crop after a refused code, and a second code uploads', async () => {
    const writes = stubGatedWrite({
      method: 'post',
      url: ICON_URL,
      retries: [INVALID_CODE, STORED],
    });
    renderEditor(iconUpload);
    await applyCrop();
    await sendCode();

    await waitFor(() => expect(writes).toHaveLength(2));
    expect(stepUpDialog()).toBeInTheDocument();
    expect(onConfirm).not.toHaveBeenCalled();

    await sendCode(FIXTURE_OTP_2);
    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(STORED_URL));
    expect(writeCodes(writes)).toEqual([FIXTURE_OTP, FIXTURE_OTP_2]);
    expect(writes[2].file?.text).toBe('crop-1');
    expect(crops).toBe(1);
  });

  // Mutant: the avatar path given a step-up seed (the purpose guard dropped from `uploadBlob`).
  it('leaves the avatar upload unchanged: no mfa_code field, and a refusal is the editor error', async () => {
    const ok = stubGatedWrite({ method: 'post', url: AVATAR_URL, first: STORED });
    renderEditor(avatarUpload);
    await applyCrop();

    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(STORED_URL));
    expect(ok).toHaveLength(1);
    expect(ok[0].fields).toEqual({});
    expect(ok[0].file).toEqual({ name: 'cropped.png', type: 'image/png', text: 'crop-1' });
  });

  it('shows the server sentence for a gate-shaped refusal on the avatar path, with no dialog', async () => {
    const writes = stubGatedWrite({ method: 'post', url: AVATAR_URL });
    renderEditor(avatarUpload);
    await applyCrop();

    expect(await screen.findByText('MFA verification required')).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
    expect(writes).toHaveLength(1);
  });

  // Mutant: the adapter widened so an unflagged 429 or a 500 opens the dialog and hides the sentence.
  it.each([
    ['too large', { status: 413, body: { error: 'File too large' } }, 'File too large'],
    [
      'an unflagged 429',
      { status: 429, body: { error: 'Rate limit exceeded' } },
      'Rate limit exceeded',
    ],
    ['a 500 with no sentence', { status: 500, body: {} }, 'Failed to upload image'],
  ])('%s keeps the editor error and opens no dialog', async (_name, first, text) => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL, first });
    renderEditor(iconUpload);
    await applyCrop();

    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
    expect(writes).toHaveLength(1);
  });

  // Mutant: the enrolment seed ignored, so a code field mounts for an actor with no factor.
  it('shows the enrolment state and sends nothing more', async () => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL, first: ENROLMENT_REQUIRED });
    renderEditor(iconUpload);
    await applyCrop();

    expect(await screen.findByText(ENROLMENT_TEXT)).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(writes).toHaveLength(1);
  });

  // Mutant: the link offered without a page to own the discard question (`setUpVerification &&` dropped).
  it('offers no setup link when the page gave no way to leave', async () => {
    stubGatedWrite({ method: 'post', url: ICON_URL, first: ENROLMENT_REQUIRED });
    renderEditor(iconUpload);
    await applyCrop();

    await screen.findByText(ENROLMENT_TEXT);
    expect(screen.queryByRole('button', { name: SETUP_LINK })).not.toBeInTheDocument();
  });

  // Mutant: `closeHost` not wired to the dialog, or the link also closing the editor behind it.
  it('hands the page a closeHost that closes the dialog and leaves the editor open', async () => {
    const onSetUpVerification = vi.fn((closeHost: () => void) => closeHost());
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL, first: ENROLMENT_REQUIRED });
    renderEditor({ ...iconUpload, onSetUpVerification });
    await applyCrop();
    await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));

    expect(onSetUpVerification).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(stepUpDialog()).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Apply' })).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
    expect(writes).toHaveLength(1);
  });

  // Mutant: the page's discard guard bypassed (the dialog closed before `closeHost` is called).
  it('keeps the dialog while the page has not yet called closeHost', async () => {
    const onSetUpVerification = vi.fn();
    stubGatedWrite({ method: 'post', url: ICON_URL, first: ENROLMENT_REQUIRED });
    renderEditor({ ...iconUpload, onSetUpVerification });
    await applyCrop();
    await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));

    expect(onSetUpVerification).toHaveBeenCalledTimes(1);
    expect(stepUpDialog()).toBeInTheDocument();
  });

  // Mutant: Cancel wired to a second request, or to the editor's own close.
  it('Cancel closes only the dialog, sends nothing more, and keeps the crop for another try', async () => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL });
    renderEditor(iconUpload);
    await applyCrop();
    await code();

    // The editor's own Cancel is inert but still queryable, so scope to the step-up dialog.
    await userEvent.click(
      within(stepUpDialog() as HTMLElement).getByRole('button', { name: 'Cancel' })
    );

    await waitFor(() => expect(stepUpDialog()).not.toBeInTheDocument());
    expect(writes).toHaveLength(1);
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Apply' })).toBeEnabled();
  });

  // Mutant: `setStepUp(null)` dropped from the clean-slate reset, so a refusal for the previous
  // picture is still up (and would re-send that picture) after another one is chosen.
  it('drops a pending refusal when a different image is chosen', async () => {
    stubGatedWrite({ method: 'post', url: ICON_URL });
    const view = renderEditor(iconUpload);
    await applyCrop();
    await code();

    view.rerender(
      <ImageCropEditor
        isOpen
        onClose={onClose}
        onConfirm={onConfirm}
        imageFile={new JsdomFile(['other'], 'other.png', { type: 'image/png' })}
        title="Crop Icon"
        cropShape={{ type: 'circle' }}
        output={{ width: 512, height: 512, quality: 0.9 }}
        upload={iconUpload}
      />
    );

    await waitFor(() => expect(stepUpDialog()).not.toBeInTheDocument());
  });

  // Mutant: the re-send built from the live `upload` prop rather than the route and fields
  // frozen at the first send (a verified code would land on another route or server).
  it('re-sends to the route and fields the first send used, even when the page changes them', async () => {
    const writes = stubGatedWrite({ method: 'post', url: ICON_URL, retries: [STORED] });
    const elsewhere = stubGatedWrite({ method: 'post', url: BANNER_URL, retries: [STORED] });
    const view = renderEditor(iconUpload);
    await applyCrop();
    await code();

    view.rerender(
      <ImageCropEditor
        isOpen
        onClose={onClose}
        onConfirm={onConfirm}
        imageFile={file}
        title="Crop Icon"
        cropShape={{ type: 'circle' }}
        output={{ width: 512, height: 512, quality: 0.9 }}
        upload={{
          ...iconUpload,
          endpoint: bannerUpload.endpoint,
          extraFields: { server_id: 'server-2' },
        }}
      />
    );
    await sendCode();

    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith(STORED_URL));
    expect(writes).toHaveLength(2);
    expect(writes[1].fields).toEqual({ server_id: 'server-1', mfa_code: FIXTURE_OTP });
    expect(elsewhere).toHaveLength(0);
  });

  // Mutant: `uploadedUrl` letting a body that does not parse reject, which the dialog reads as
  // a lost connection that "may have acted" after the server stored the image.
  it('reads a 2xx re-send whose body does not parse as storing nothing, never as a lost connection', async () => {
    let sends = 0;
    mswServer.use(
      http.post(ICON_URL, () =>
        ++sends === 1
          ? HttpResponse.json(MFA_REQUIRED.body, { status: MFA_REQUIRED.status })
          : new HttpResponse('<html>stored</html>', {
              status: 200,
              headers: { 'Content-Type': 'text/html' },
            })
      )
    );
    renderEditor(iconUpload);
    await applyCrop();
    await sendCode();

    expect(
      await screen.findByText('Upload succeeded but response did not include image URL')
    ).toBeInTheDocument();
    expect(screen.queryByText(/Couldn't reach the server/)).not.toBeInTheDocument();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  // Mutant: the refused body read with no null guard: a JSON `null` threw a TypeError whose own
  // text stood in the editor in place of ours.
  it('words a refusal whose body is JSON null with the editor sentence', async () => {
    mswServer.use(http.post(AVATAR_URL, () => HttpResponse.json(null, { status: 500 })));
    renderEditor(avatarUpload);
    await applyCrop();

    expect(await screen.findByText('Failed to upload image')).toBeInTheDocument();
  });

  // Mutant: a 200 without a URL on the re-send read as success (the editor would confirm `null`).
  it('treats a re-send that stored nothing as a failure in the dialog, not a success', async () => {
    stubGatedWrite({ method: 'post', url: ICON_URL, retries: [{ status: 200, body: {} }] });
    renderEditor(iconUpload);
    await applyCrop();
    await sendCode();

    expect(
      await screen.findByText('Upload succeeded but response did not include image URL')
    ).toBeInTheDocument();
    expect(onConfirm).not.toHaveBeenCalled();
    expect(stepUpDialog()).toBeInTheDocument();
  });
});
