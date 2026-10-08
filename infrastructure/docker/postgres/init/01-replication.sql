-- Enable replication for the concord user.
-- PostgreSQL requires the REPLICATION attribute and pg_hba entry
-- for streaming replication from the replica on port 5433.
ALTER ROLE concord REPLICATION;
