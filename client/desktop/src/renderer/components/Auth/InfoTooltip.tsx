import React, { useState } from 'react';
import './InfoTooltip.css';

export interface InfoTooltipProps {
  content: React.ReactNode;
}

const InfoTooltip: React.FC<InfoTooltipProps> = ({ content }) => {
  const [isVisible, setIsVisible] = useState(false);

  return (
    <div className="info-tooltip-container">
      <button
        type="button"
        className="info-tooltip-icon"
        onMouseEnter={() => setIsVisible(true)}
        onMouseLeave={() => setIsVisible(false)}
        onClick={(e) => {
          e.preventDefault();
          setIsVisible(!isVisible);
        }}
        aria-label="More information"
      >
        <svg
          width="16"
          height="16"
          viewBox="0 0 16 16"
          fill="none"
          xmlns="http://www.w3.org/2000/svg"
        >
          <circle cx="8" cy="8" r="7" stroke="currentColor" strokeWidth="1.5" fill="none" />
          <path
            d="M8 11V7.5M8 5.5H8.005"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
        </svg>
      </button>

      {isVisible && (
        <div className="info-tooltip-popup">
          <div className="info-tooltip-content">{content}</div>
        </div>
      )}
    </div>
  );
};

export default InfoTooltip;
