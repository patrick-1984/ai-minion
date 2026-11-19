-- The cached write result remembers a digest of the request body (A49 T14):
-- a duplicate request id with the same body is answered from the cache, one
-- with a different body is refused. The digest is SHA-256 of the canonical
-- JSON of the arguments without request_id; it never holds the content. Rows
-- written before this migration have no digest and are matched as before.
ALTER TABLE op_results ADD COLUMN args_sha256 TEXT NOT NULL DEFAULT '';
