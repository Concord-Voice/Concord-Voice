import React from 'react';
import './PasswordStrength.css';

export interface PasswordStrengthProps {
  password: string;
}

interface StrengthResult {
  score: number; // 0-5
  label: string;
  color: string;
  feedback: string[];
}

function getVarietyFeedback(hasLower: boolean, hasUpper: boolean, hasNumbers: boolean): string {
  if (!hasLower || !hasUpper) return 'Mix uppercase and lowercase';
  if (!hasNumbers) return 'Add some numbers';
  return 'Add special characters (!@#$%...)';
}

function scoreLengthAndVariety(password: string): {
  score: number;
  feedback: string[];
  varietyCount: number;
} {
  let score = 0;
  const feedback: string[] = [];

  if (password.length >= 12) score++;
  if (password.length >= 16) score++;
  if (password.length >= 20) score++;
  if (password.length >= 24) score++;

  if (password.length < 12) {
    feedback.push('Use at least 12 characters');
  } else if (password.length < 16) {
    feedback.push('Try 16+ characters for better security');
  }

  const hasLowercase = /[a-z]/.test(password);
  const hasUppercase = /[A-Z]/.test(password);
  const hasNumbers = /\d/.test(password);
  const hasSpecial = /[^a-zA-Z0-9]/.test(password);
  const varietyCount = [hasLowercase, hasUppercase, hasNumbers, hasSpecial].filter(Boolean).length;

  if (varietyCount >= 2) score++;
  if (varietyCount >= 4) score++;
  if (varietyCount < 3) feedback.push(getVarietyFeedback(hasLowercase, hasUppercase, hasNumbers));

  return { score, feedback, varietyCount };
}

const commonPatterns = [
  { pattern: /^(123|abc|qwe)/i, message: 'Avoid sequential starts' },
  { pattern: /password|letmein|welcome/i, message: 'Avoid common words' },
  { pattern: /(.)\1{2,}/, message: 'Avoid repeated characters' },
  { pattern: /^[a-z]+$/i, message: 'Add numbers or symbols' },
  { pattern: /^\d+$/, message: 'Add letters' },
];

function calculateStrength(password: string): StrengthResult {
  if (!password) {
    return {
      score: 0,
      label: 'No password',
      color: 'var(--text-muted)',
      feedback: ['Enter a password'],
    };
  }

  const { score: baseScore, feedback, varietyCount } = scoreLengthAndVariety(password);
  let score = baseScore;
  let hasPenalty = false;

  for (const { pattern, message } of commonPatterns) {
    if (pattern.test(password)) {
      score = Math.max(0, score - 2);
      feedback.push(message);
      hasPenalty = true;
      break;
    }
  }

  if (score >= 6 && !hasPenalty && password.length >= 24 && varietyCount === 4) {
    score = 5;
  }

  score = Math.max(0, Math.min(5, score));

  // Determine label and color based on score
  const levels = [
    { label: 'Very Weak', color: 'var(--danger)' },
    { label: 'Weak', color: '#f39c12' },
    { label: 'Fair', color: 'var(--accent-secondary)' },
    { label: 'Good', color: '#43b581' },
    { label: 'Strong', color: 'var(--success)' },
    { label: 'Legendary 🔥', color: '#b762ff' }, // Purple for legendary tier
  ];

  return {
    score,
    label: levels[score].label,
    color: levels[score].color,
    feedback: feedback.slice(0, 2), // Show max 2 feedback items
  };
}

const PasswordStrength: React.FC<PasswordStrengthProps> = ({ password }) => {
  const strength = calculateStrength(password);

  if (!password) {
    return null;
  }

  return (
    <div className="password-strength">
      <div className="strength-bar-container">
        {['seg-1', 'seg-2', 'seg-3', 'seg-4', 'seg-5'].map((id, index) => (
          <div
            key={id}
            className={`strength-bar-segment ${index < strength.score ? 'active' : ''}`}
            style={{
              backgroundColor: index < strength.score ? strength.color : 'var(--bg-tertiary)',
            }}
          />
        ))}
      </div>

      <div className="strength-info">
        <span
          className={`strength-label ${strength.score === 5 ? 'legendary' : ''}`}
          style={{ color: strength.score === 5 ? undefined : strength.color }}
        >
          {strength.label}
        </span>
        {strength.feedback.length > 0 && (
          <div className="strength-feedback">
            {strength.feedback.map((tip) => (
              <span key={tip} className="feedback-item">
                {tip}
              </span>
            ))}
          </div>
        )}
      </div>
    </div>
  );
};

export default PasswordStrength;
