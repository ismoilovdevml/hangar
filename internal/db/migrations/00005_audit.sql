-- +goose Up
-- Append-only audit log, range-partitioned by month on "at".
-- Retention detaches/drops whole partitions; rows are never updated or deleted.

CREATE SEQUENCE audit_events_id_seq;

CREATE TABLE audit_events (
    id      bigint NOT NULL DEFAULT nextval('audit_events_id_seq'),
    at      timestamptz NOT NULL DEFAULT now(),
    actor   text NOT NULL,
    action  text NOT NULL,
    target  text NOT NULL DEFAULT '',
    outcome text NOT NULL DEFAULT 'success' CHECK (outcome IN ('success', 'denied', 'error')),
    ip      inet,
    details jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);
ALTER SEQUENCE audit_events_id_seq OWNED BY audit_events.id;

-- Catches rows outside the prepared monthly partitions so inserts never fail.
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;

CREATE INDEX audit_events_at_idx ON audit_events (at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor, at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, at DESC);

-- Creates monthly partitions audit_events_YYYYMM from the current month up to
-- months_ahead months ahead. Idempotent; the scheduler calls it daily.
-- +goose StatementBegin
CREATE FUNCTION hangar_ensure_audit_partitions(months_ahead integer DEFAULT 2)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    m       date := date_trunc('month', now())::date;
    created integer := 0;
    part    text;
BEGIN
    FOR i IN 0..months_ahead LOOP
        part := format('audit_events_%s', to_char(m, 'YYYYMM'));
        IF to_regclass(part) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF audit_events FOR VALUES FROM (%L) TO (%L)',
                part, m, (m + interval '1 month')::date);
            created := created + 1;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN created;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_audit_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION hangar_audit_append_only();

SELECT hangar_ensure_audit_partitions(2);

-- +goose Down
DROP TABLE audit_events;
DROP FUNCTION hangar_audit_append_only();
DROP FUNCTION hangar_ensure_audit_partitions(integer);
