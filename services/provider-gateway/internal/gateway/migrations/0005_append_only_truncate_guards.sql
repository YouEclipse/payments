-- These guards make ordinary application and migration-role mutation paths
-- fail closed. A database owner can still DROP triggers; control of that role
-- is therefore an explicit operational trust boundary.
UPDATE provider_gateway.operations
SET updated_at = created_at
WHERE updated_at < created_at;

CREATE FUNCTION provider_gateway.reject_provider_connection_revision_removal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'provider connection revision bindings cannot be removed' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER provider_connection_revision_binding_no_delete
BEFORE DELETE ON provider_gateway.provider_connection_revisions
FOR EACH ROW EXECUTE FUNCTION provider_gateway.reject_provider_connection_revision_removal();

CREATE TRIGGER provider_connection_revision_binding_no_truncate
BEFORE TRUNCATE ON provider_gateway.provider_connection_revisions
FOR EACH STATEMENT EXECUTE FUNCTION provider_gateway.reject_provider_connection_revision_removal();

CREATE FUNCTION provider_gateway.reject_provider_operation_observation_truncate() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'provider operation observations are append-only' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER provider_operation_observations_no_truncate
BEFORE TRUNCATE ON provider_gateway.provider_operation_observations
FOR EACH STATEMENT EXECUTE FUNCTION provider_gateway.reject_provider_operation_observation_truncate();
