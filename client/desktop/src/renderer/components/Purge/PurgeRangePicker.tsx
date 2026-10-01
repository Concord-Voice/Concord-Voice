import React, { useId } from 'react';
import { PURGE_RANGES, PURGE_RANGE_LABELS, type PurgeRange } from '../../constants/purgeRanges';

interface PurgeRangePickerProps {
  value: PurgeRange | null;
  onChange: (range: PurgeRange) => void;
  firstOptionRef?: React.RefObject<HTMLSelectElement | null>;
  /** Server context only. Copy deck §3. */
  helper?: string;
}

/**
 * A native select rather than nine radios: the chosen range reads as one
 * value, and keyboard and screen-reader behaviour come from the platform.
 * The placeholder keeps "no range preselected" — confirm stays disabled until
 * the user picks one.
 */
const PurgeRangePicker: React.FC<PurgeRangePickerProps> = ({
  value,
  onChange,
  firstOptionRef,
  helper,
}) => {
  // Per instance, so two mounted pickers never share one label target.
  const selectId = useId();
  return (
    <div className="purge-modal__ranges">
      <label htmlFor={selectId}>Range</label>
      <select
        id={selectId}
        ref={firstOptionRef}
        className="form-input purge-modal__range-select"
        value={value ?? ''}
        onChange={(event) => onChange(event.target.value as PurgeRange)}
      >
        <option value="" disabled>
          Choose a range…
        </option>
        {PURGE_RANGES.map((range) => (
          // The `all` option's name carries "no time limit" so the qualifier is
          // part of the value a screen reader announces. Copy deck §3.
          <option key={range} value={range}>
            {range === 'all' ? 'All messages — no time limit' : PURGE_RANGE_LABELS[range]}
          </option>
        ))}
      </select>
      {helper !== undefined && <p className="purge-modal__ranges-helper">{helper}</p>}
    </div>
  );
};

export default PurgeRangePicker;
