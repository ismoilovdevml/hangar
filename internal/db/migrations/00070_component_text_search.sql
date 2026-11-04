-- +goose Up
-- Full-text document over a component's coordinates and string attributes,
-- used by format search endpoints (NuGet) instead of listing every
-- component. Tokens are runs of letters and digits, so "Newtonsoft.Json"
-- yields "newtonsoft" and "json". The text is capped so that a component
-- with very large attributes can never exceed the tsvector size limit and
-- fail its publish.

-- +goose StatementBegin
CREATE FUNCTION hangar_component_document(namespace text, name text, attributes jsonb)
RETURNS tsvector
LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN to_tsvector('simple'::regconfig,
    regexp_replace(lower(left(namespace || ' ' || name || ' ' || attributes::text, 100000)),
                   '[^[:alnum:]]+', ' ', 'g'));
-- +goose StatementEnd

CREATE INDEX components_document_idx ON components
    USING gin (hangar_component_document(namespace, name, attributes));

-- +goose Down
DROP INDEX components_document_idx;
DROP FUNCTION hangar_component_document(text, text, jsonb);
