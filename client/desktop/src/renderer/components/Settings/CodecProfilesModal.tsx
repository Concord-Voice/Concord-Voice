import React from 'react';
import Modal from '../ui/Modal';
import { CODEC_PROFILE_GUIDE } from './codecMetadata';
import './CodecProfilesModal.css';

export interface CodecProfilesModalProps {
  isOpen: boolean;
  onClose: () => void;
}

const CodecProfilesModal: React.FC<CodecProfilesModalProps> = ({ isOpen, onClose }) => (
  <Modal isOpen={isOpen} onClose={onClose} title="What are codec profiles?" width="large">
    <p className="codec-profiles-intro">
      A codec defines how Concord compresses video. A profile selects features within that codec,
      while a level describes its maximum advertised workload. A level does not force a particular
      resolution, frame rate, or bitrate. Concord may negotiate a compatible fallback for a call.
    </p>

    <div className="codec-profiles-table-wrap">
      <table className="codec-profiles-table">
        <caption>Codec profiles available in Concord</caption>
        <thead>
          <tr>
            <th scope="col">Menu label</th>
            <th scope="col">Standards mapping</th>
            <th scope="col">Practical meaning</th>
          </tr>
        </thead>
        <tbody>
          {CODEC_PROFILE_GUIDE.map((profile) => (
            <tr key={profile.key}>
              <th scope="row">{profile.label}</th>
              <td>
                {profile.standard}
                {profile.signal && (
                  <>
                    <br />
                    <code>{profile.signal}</code>
                  </>
                )}
              </td>
              <td>{profile.meaning}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>

    <div className="codec-profiles-notes">
      <p>
        H.264 High is an 8-bit SDR profile. H.264 High 10 is a different profile and is not
        available through Concord&apos;s current WebRTC stack.
      </p>
      <p>
        HEVC/H.265 may be detected by your system, but system detection does not make it routable in
        Concord. It remains unavailable.
      </p>
    </div>
  </Modal>
);

export default CodecProfilesModal;
