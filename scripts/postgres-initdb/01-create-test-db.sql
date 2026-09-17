-- Runs once, on a fresh postgres-data volume only (postgres-initdb scripts
-- never re-run against an already-initialized data directory). Provisions
-- a second database so integration tests never point at the same
-- database local development actually uses.
CREATE DATABASE aggregator_test OWNER aggregator;
