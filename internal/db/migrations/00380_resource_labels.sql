-- +goose Up
-- Labels on configuration resources: repositories, blob stores, cleanup
-- policies, roles and outbound proxies. `hangar apply` keeps its ownership
-- marks here (hangar/managed-by, hangar/apply-set), so `--prune` only ever
-- deletes what an apply created or adopted.
--
-- A row belongs to the resource it was written for: resource_id is that
-- resource's identity (its id; for outbound proxies, which have none, the
-- creation time). Reads join on it, so a resource deleted and created again
-- under the same name starts without labels and is never pruned on the
-- strength of its predecessor's marks. Rows of deleted resources are inert
-- and are replaced when the name is labelled again.
CREATE TABLE resource_labels (
    kind        text NOT NULL CHECK (kind IN ('repository', 'blob_store', 'cleanup_policy', 'role', 'proxy')),
    name        text NOT NULL,
    resource_id text NOT NULL,
    labels      jsonb NOT NULL CHECK (jsonb_typeof(labels) = 'object'),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  text NOT NULL DEFAULT '',
    PRIMARY KEY (kind, name)
);

-- +goose Down
DROP TABLE resource_labels;
