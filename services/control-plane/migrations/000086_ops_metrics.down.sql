-- Migration: ops_metrics (down)
-- Purpose: Remove aggregate operations samples and hourly rollups.

DROP TABLE IF EXISTS ops_metric_rollups;
DROP INDEX IF EXISTS idx_ops_metric_samples_ts_brin;
DROP TABLE IF EXISTS ops_metric_samples;
