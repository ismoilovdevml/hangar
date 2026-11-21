-- +goose Up
-- Audit log partition maintenance.
--
-- A row whose month had no partition yet (a node with a clock months ahead,
-- or a scheduler that did not run for months) lands in audit_events_default.
-- Creating that month's partition then fails ("updated partition constraint
-- for default partition would be violated"), and because the failure aborts
-- hangar_ensure_audit_partitions, no later month got a partition either:
-- from then on every event went to the default partition. Now such rows are
-- moved into their month's partition as it is created, and every month that
-- has rows in the default partition gets its partition, so the default
-- partition empties itself on the next run.
--
-- Month boundaries follow the session time zone, as they always have.

-- hangar_create_audit_partition creates the partition of month m (its first
-- day) unless it exists, moving that month's rows out of the default
-- partition first. Returns 1 if it created the partition, else 0.
-- +goose StatementBegin
CREATE FUNCTION hangar_create_audit_partition(m date)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    part text := format('audit_events_%s', to_char(m, 'YYYYMM'));
    hi   date := (m + interval '1 month')::date;
BEGIN
    IF to_regclass(part) IS NOT NULL THEN
        RETURN 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM audit_events_default WHERE at >= m AND at < hi) THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF audit_events FOR VALUES FROM (%L) TO (%L)', part, m, hi);
        RETURN 1;
    END IF;
    -- The rows are moved, not changed: the append-only trigger is lifted
    -- on the default partition for this one statement, inside the caller's
    -- transaction, and holds again when the transaction ends either way.
    EXECUTE format('CREATE TABLE %I (LIKE audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)', part);
    ALTER TABLE audit_events_default DISABLE TRIGGER audit_events_append_only;
    EXECUTE format('WITH moved AS (DELETE FROM audit_events_default WHERE at >= %L AND at < %L RETURNING *)
        INSERT INTO %I SELECT * FROM moved', m, hi, part);
    ALTER TABLE audit_events_default ENABLE TRIGGER audit_events_append_only;
    EXECUTE format('ALTER TABLE audit_events ATTACH PARTITION %I FOR VALUES FROM (%L) TO (%L)', part, m, hi);
    RETURN 1;
END;
$$;
-- +goose StatementEnd

-- hangar_ensure_audit_partitions creates the partitions of the current month,
-- of months_ahead following months, and of every month with rows in the
-- default partition. Idempotent; returns the number of partitions created.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION hangar_ensure_audit_partitions(months_ahead integer DEFAULT 2)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    months  date[];
    m       date;
    created integer := 0;
BEGIN
    -- Collected first: moving rows alters the default partition, which an
    -- open loop over it would forbid.
    SELECT array_agg(x ORDER BY x) INTO months FROM (
        SELECT (date_trunc('month', now()) + make_interval(months => i))::date FROM generate_series(0, months_ahead) i
        UNION
        SELECT DISTINCT date_trunc('month', at)::date FROM audit_events_default
    ) s(x);
    FOREACH m IN ARRAY months LOOP
        created := created + hangar_create_audit_partition(m);
    END LOOP;
    RETURN created;
END;
$$;
-- +goose StatementEnd

-- hangar_drop_audit_partitions drops the monthly partitions whose month ended
-- before cutoff and that hold no row at or after it, and returns their names.
-- Retention removes whole partitions; rows are never deleted one by one.
-- +goose StatementBegin
CREATE FUNCTION hangar_drop_audit_partitions(cutoff timestamptz)
RETURNS SETOF text
LANGUAGE plpgsql
AS $$
DECLARE
    part   text;
    newest timestamptz;
BEGIN
    FOR part IN
        SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = 'audit_events'::regclass AND c.relname ~ '^audit_events_[0-9]{6}$'
           AND (to_date(right(c.relname, 6), 'YYYYMM') + interval '1 month')::date <= cutoff
         ORDER BY c.relname
    LOOP
        EXECUTE format('SELECT max(at) FROM %I', part) INTO newest;
        IF newest IS NULL OR newest < cutoff THEN
            EXECUTE format('DROP TABLE %I', part);
            RETURN NEXT part;
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION hangar_drop_audit_partitions(timestamptz);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION hangar_ensure_audit_partitions(months_ahead integer DEFAULT 2)
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

DROP FUNCTION hangar_create_audit_partition(date);
