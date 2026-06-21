import React from 'react';
import './LoadingSpinner.css';

export interface LoadingSpinnerProps {
  size?: 'small' | 'medium' | 'large';
  inline?: boolean;
}

const LoadingSpinner: React.FC<LoadingSpinnerProps> = ({ size = 'medium', inline = false }) => {
  return (
    <div className={`loading-spinner loading-spinner-${size} ${inline ? 'inline' : ''}`}>
      <div className="spinner"></div>
    </div>
  );
};

export default LoadingSpinner;
