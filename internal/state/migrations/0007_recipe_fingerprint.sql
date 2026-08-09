-- Runstead digest-bound recipe approval identity, migration 7 (issue #26 review).
--
--   - actions.recipe_fingerprint: the digest-bound approval identity of a
--     run_recipe proposal. It binds the recipe id to the effective definition
--     digest (executable, argv, cwd, capabilities, allowed env, timeout and
--     output limits), so an operator approval for one definition can never
--     authorize a different definition of the same id. The plain fingerprint
--     column remains the repeat/loop evidence; approval lookups use the
--     recipe fingerprint for recipe actions and the plain fingerprint for
--     everything else.

ALTER TABLE actions ADD COLUMN recipe_fingerprint TEXT NOT NULL DEFAULT '';
