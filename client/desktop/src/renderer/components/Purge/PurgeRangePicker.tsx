import React from 'react';
import { PURGE_RANGES, PURGE_RANGE_LABELS, type PurgeRange } from '../../constants/purgeRanges';

interface PurgeRangePickerProps {
  value: PurgeRange | null;
  onChange: (range: PurgeRange) => void;
  firstOptionRef?: React.RefObject<HTMLInputElement | null>;
  /** Server context only. Copy deck §3. */
  helper?: string;
}

const PurgeRangePicker: React.FC<PurgeRangePickerProps> = ({
  value,
  onChange,
  firstOptionRef,
  helper,
}) => (
  <fieldset className="purge-modal__ranges">
    <legend>Range</legend>
    {PURGE_RANGES.map((range, index) => (
      <label
        key={range}
        className={`purge-modal__range${range === 'all' ? ' purge-modal__range--all' : ''}`}
      >
        <input
          ref={index === 0 ? firstOptionRef : undefined}
          type="radio"
          name="purge-range"
          value={range}
          checked={value === range}
          onChange={() => onChange(range)}
        />
        {/* The `all` option's accessible name carries "no time limit" because the
            visual treatment (gap plus warning glyph) is not available to a
            screen reader. Copy deck §3. */}
        <span>{range === 'all' ? 'All messages — no time limit' : PURGE_RANGE_LABELS[range]}</span>
      </label>
    ))}
    {helper !== undefined && <p className="purge-modal__ranges-helper">{helper}</p>}
  </fieldset>
);

export default PurgeRangePicker;
