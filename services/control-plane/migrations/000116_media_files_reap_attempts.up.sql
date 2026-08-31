-- P3 straggler fairness: repeated storage-delete failures must not pin the
-- oldest stride while newer attachment blobs wait behind them.
ALTER TABLE media_files
    ADD COLUMN IF NOT EXISTS reap_attempts INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN media_files.reap_attempts IS
    'Number of failed straggler-sweep attempts for this blob; ordering by this counter lets repeatedly failing deletes yield to newer candidates.';
