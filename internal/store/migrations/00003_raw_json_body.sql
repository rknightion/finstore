-- +goose Up
-- raw.json_body reads a stored response body as JSON, or returns null when the body is not UTF-8
-- or not JSON. raw keeps bodies exactly as received (load-capture does not check them), so a view
-- that reads bodies must never fail on one it cannot read. Only the agent views call it: they run
-- as finstore_owner, and finstore_agent has no USAGE on raw, so it cannot name the function.
-- +goose StatementBegin
CREATE FUNCTION raw.json_body(body bytea) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
BEGIN
  RETURN convert_from(body, 'UTF8')::jsonb;
EXCEPTION WHEN data_exception THEN
  -- invalid UTF-8 (22021), invalid JSON (22P02), a \u0000 escape jsonb refuses (22P05)
  RETURN NULL;
END
$$;
-- +goose StatementEnd
