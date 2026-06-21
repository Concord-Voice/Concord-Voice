import { useState, useCallback } from 'react';

/**
 * Shared form-state hook that extracts the common error / submitting / success
 * boilerplate duplicated across modal forms and page forms.
 *
 * Usage:
 *   const { errors, setErrors, isSubmitting, setIsSubmitting,
 *           successMessage, setSuccessMessage, clearError, reset } =
 *     useFormState<MyFormErrors>();
 */
export function useFormState<E extends { [K in keyof E]?: string }>(initialErrors?: E) {
  const [errors, setErrors] = useState<E>(initialErrors ?? ({} as E));
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);

  /** Clear a single field error (useful in onChange handlers) */
  const clearError = useCallback((field: keyof E) => {
    setErrors((prev) => {
      if (!prev[field]) return prev;
      return { ...prev, [field]: undefined };
    });
  }, []);

  /** Reset errors, submitting, and success back to initial state */
  const reset = useCallback(() => {
    setErrors(initialErrors ?? ({} as E));
    setIsSubmitting(false);
    setSuccessMessage(null);
  }, [initialErrors]);

  return {
    errors,
    setErrors,
    isSubmitting,
    setIsSubmitting,
    successMessage,
    setSuccessMessage,
    clearError,
    reset,
  };
}
