-- Functions the insight views and the CLI share. Applied by applyViewsAndGrants after
-- agent_views.sql and before grants.sql, as finstore_owner. A later change to the argument names
-- or return type needs an explicit DROP FUNCTION first (after the views are dropped), because
-- CREATE OR REPLACE cannot change them.

CREATE OR REPLACE FUNCTION agent.normalise_payee_name(p_name text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $fn$
DECLARE
  s    text := lower(p_name);
  toks text[];
  n    int;
BEGIN
  -- Date tokens go first, while their separators are still there: 2026-11-01, 01/11/2026, 01nov26.
  s := regexp_replace(s, '\m\d{4}-\d{1,2}-\d{1,2}\M', ' ', 'g');
  s := regexp_replace(s, '\m\d{1,2}/\d{1,2}/\d{2,4}\M', ' ', 'g');
  s := regexp_replace(s, '\m\d{1,2}(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)(\d{4}|\d{2})\M', ' ', 'g');
  -- Rule 1: runs of anything but letters, digits, & and ' become one space.
  s := regexp_replace(s, '[^[:alnum:]&'' ]+', ' ', 'g');
  toks := array_remove(regexp_split_to_array(btrim(s), '\s+'), '');
  -- Rule 2: drop reference tokens (six or more characters containing a digit) ...
  toks := ARRAY(SELECT t FROM unnest(toks) WITH ORDINALITY AS u(t, o)
                WHERE NOT (length(t) >= 6 AND t ~ '[0-9]') ORDER BY o);
  n := coalesce(array_length(toks, 1), 0);
  -- ... and a trailing run of all-digit tokens.
  WHILE n > 0 AND toks[n] ~ '^[0-9]+$' LOOP
    toks := toks[1:n - 1];
    n := n - 1;
  END LOOP;
  -- Rule 3: a leading '<word> re ' collector prefix, when a merchant follows it.
  IF n >= 3 AND toks[2] = 're' THEN
    toks := toks[3:n];
    n := n - 2;
  END IF;
  -- Rule 4: trailing company suffixes.
  WHILE n > 0 AND toks[n] IN ('ltd', 'limited', 'plc', 'llp', 'inc', 'uk') LOOP
    toks := toks[1:n - 1];
    n := n - 1;
  END LOOP;
  -- Rule 5: an empty result is NULL.
  RETURN nullif(array_to_string(toks, ' '), '');
END
$fn$;

COMMENT ON FUNCTION agent.normalise_payee_name(text) IS
  'The payee name key for n: payees. Rules, applied in order to the lowercased name: (0) date tokens (2026-11-01, 01/11/2026, 01nov26) are dropped; (1) every run of characters other than letters, digits, & and apostrophe becomes one space; (2) tokens of six or more characters containing a digit are dropped, then a trailing run of all-digit tokens; (3) a leading "<word> re " collector prefix is dropped when a merchant follows it; (4) trailing ltd, limited, plc, llp, inc and uk tokens are dropped; (5) the result is trimmed, and an empty result, like a null name, returns NULL so a view can fall through to the x: key. These are tuning values: a change needs a test reporting which fixture keys change.';

-- After the function, not in grants.sql: Migrate runs grants.sql alone when the stored revision
-- differs, before any function exists. The explicit grant keeps the views working for the three
-- roles if the default PUBLIC grant is ever revoked.
GRANT EXECUTE ON FUNCTION agent.normalise_payee_name(text) TO finstore_app, finstore_agent, finstore_mcp;
