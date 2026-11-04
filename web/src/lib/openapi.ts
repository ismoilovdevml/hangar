/**
 * Reading the server's OpenAPI 3.1 document for the API reference page:
 * operations grouped by tag, schemas as property tables, example values and
 * curl commands. Pure functions only; only the parts Hangar's document uses
 * are supported.
 */

export interface Schema {
  $ref?: string;
  type?: string | string[];
  format?: string;
  description?: string;
  properties?: Record<string, Schema>;
  required?: string[];
  items?: Schema;
  enum?: unknown[];
  const?: unknown;
  example?: unknown;
  examples?: unknown[];
  default?: unknown;
  allOf?: Schema[];
  oneOf?: Schema[];
  anyOf?: Schema[];
  additionalProperties?: boolean | Schema;
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  minItems?: number;
  maxItems?: number;
  pattern?: string;
  readOnly?: boolean;
  writeOnly?: boolean;
}

/** A reference to a component: "#/components/schemas/Token". */
export interface Ref {
  $ref: string;
}

export interface Parameter {
  name: string;
  in: 'path' | 'query' | 'header' | 'cookie';
  description?: string;
  required?: boolean;
  schema?: Schema;
}

interface MediaType {
  schema?: Schema;
}

interface RequestBody {
  required?: boolean;
  description?: string;
  content?: Record<string, MediaType>;
}

interface Response {
  description?: string;
  headers?: Record<string, unknown>;
  content?: Record<string, MediaType>;
}

interface RawOperation {
  tags?: string[];
  operationId?: string;
  summary?: string;
  description?: string;
  parameters?: (Parameter | Ref)[];
  requestBody?: RequestBody | Ref;
  responses?: Record<string, Response | Ref>;
  security?: unknown[];
  deprecated?: boolean;
}

type PathItem = Partial<Record<(typeof METHODS)[number], RawOperation>> & { parameters?: (Parameter | Ref)[] };

export interface OpenAPIDoc {
  openapi: string;
  info: { title: string; version: string; summary?: string; description?: string };
  tags?: { name: string; description?: string }[];
  paths: Record<string, PathItem>;
  components?: {
    schemas?: Record<string, Schema>;
    parameters?: Record<string, Parameter>;
    responses?: Record<string, Response>;
    securitySchemes?: Record<string, { type: string; scheme?: string; description?: string; name?: string; in?: string }>;
  };
}

export interface OperationResponse {
  status: string;
  description: string;
  contentType?: string;
  schema?: Schema;
}

export interface Operation {
  /** operationId, or method and path when the document has none. */
  id: string;
  method: string;
  path: string;
  tag: string;
  summary: string;
  description: string;
  parameters: Parameter[];
  body?: { required: boolean; contentType: string; schema?: Schema; description?: string };
  responses: OperationResponse[];
  /** Callable without signing in (`security: []`). */
  public: boolean;
  deprecated: boolean;
}

const METHODS = ['get', 'post', 'put', 'patch', 'delete'] as const;

/** Follows a local `$ref` ("#/components/schemas/Token"); unknown refs resolve to an empty object. */
export function resolveRef<T extends object>(doc: OpenAPIDoc, obj: T | Ref | undefined, depth = 0): T | undefined {
  const ref = obj && '$ref' in obj ? obj.$ref : undefined;
  if (typeof ref !== 'string' || depth > 10) return obj as T | undefined;
  if (!ref.startsWith('#/')) return {} as T;
  let cur: unknown = doc;
  for (const part of ref.slice(2).split('/')) {
    cur = cur && typeof cur === 'object' ? (cur as Record<string, unknown>)[part.replace(/~1/g, '/').replace(/~0/g, '~')] : undefined;
  }
  return resolveRef<T>(doc, (cur ?? {}) as T, depth + 1);
}

/** The schema name of a `$ref`, e.g. "Token". */
export function refName(ref: string): string {
  return ref.slice(ref.lastIndexOf('/') + 1);
}

function firstContent(content: Record<string, MediaType> | undefined): { contentType?: string; schema?: Schema } {
  if (!content) return {};
  const types = Object.keys(content);
  const ct = types.find((t) => t === 'application/json') ?? types[0];
  return ct ? { contentType: ct, schema: content[ct]?.schema } : {};
}

/** Every operation in document order, path-level parameters merged in. */
export function listOperations(doc: OpenAPIDoc): Operation[] {
  const out: Operation[] = [];
  for (const [path, item] of Object.entries(doc.paths ?? {})) {
    const shared = (item.parameters ?? []).map((p) => resolveRef<Parameter>(doc, p)!);
    for (const method of METHODS) {
      const op = item[method];
      if (!op) continue;
      const own = (op.parameters ?? []).map((p) => resolveRef<Parameter>(doc, p)!);
      const params = [...shared.filter((s) => !own.some((o) => o.name === s.name && o.in === s.in)), ...own];
      const rb = resolveRef<RequestBody>(doc, op.requestBody);
      const body = rb ? { required: !!rb.required, description: rb.description, ...firstContent(rb.content) } : undefined;
      const responses = Object.entries(op.responses ?? {}).map(([status, r]) => {
        const res = resolveRef<Response>(doc, r) ?? {};
        return { status, description: res.description ?? '', ...firstContent(res.content) };
      });
      out.push({
        id: op.operationId ?? `${method}-${path}`,
        method: method.toUpperCase(),
        path,
        tag: op.tags?.[0] ?? 'other',
        summary: op.summary ?? '',
        description: op.description ?? '',
        parameters: params,
        body: body ? { ...body, contentType: body.contentType ?? 'application/json' } : undefined,
        responses,
        public: Array.isArray(op.security) && op.security.length === 0,
        deprecated: !!op.deprecated,
      });
    }
  }
  return out;
}

export interface TagGroup {
  tag: string;
  description: string;
  operations: Operation[];
}

/** Operations by their first tag, in the document's tag order; untagged ones last. */
export function groupByTag(doc: OpenAPIDoc, ops: Operation[]): TagGroup[] {
  const order = (doc.tags ?? []).map((t) => t.name);
  const groups = new Map<string, TagGroup>();
  for (const name of order) groups.set(name, { tag: name, description: doc.tags?.find((t) => t.name === name)?.description ?? '', operations: [] });
  for (const op of ops) {
    if (!groups.has(op.tag)) groups.set(op.tag, { tag: op.tag, description: '', operations: [] });
    groups.get(op.tag)!.operations.push(op);
  }
  return [...groups.values()].filter((g) => g.operations.length > 0);
}

/** Operations matching every word of q in their method, path, summary, id, tag or description. */
export function filterOperations(ops: Operation[], q: string): Operation[] {
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return ops;
  return ops.filter((op) => {
    const hay = `${op.method} ${op.path} ${op.summary} ${op.id} ${op.tag} ${op.description}`.toLowerCase();
    return words.every((w) => hay.includes(w));
  });
}

/** A short type for tables: "string", "Token[]", "integer | null", "\"a\" | \"b\"". */
export function typeLabel(doc: OpenAPIDoc, schema: Schema | undefined, depth = 0): string {
  if (!schema) return 'any';
  if (depth > 4) return 'object';
  if (schema.$ref) return refName(schema.$ref);
  if (schema.enum) {
    const vals = schema.enum.map((v) => JSON.stringify(v));
    return vals.length > 6 ? `${vals.slice(0, 6).join(' | ')} | …` : vals.join(' | ');
  }
  const union = schema.allOf?.length === 1 ? schema.allOf : (schema.oneOf ?? schema.anyOf);
  if (union) return union.map((s) => typeLabel(doc, s, depth + 1)).join(' | ');
  const types = Array.isArray(schema.type) ? schema.type : schema.type ? [schema.type] : [];
  if (types.length === 0) return schema.properties ? 'object' : 'any';
  return types
    .map((t) => {
      if (t === 'array') return `${typeLabel(doc, schema.items, depth + 1)}[]`;
      if (t === 'string' && schema.format) return `string (${schema.format})`;
      return t;
    })
    .join(' | ');
}

export interface PropertyRow {
  name: string;
  type: string;
  required: boolean;
  description: string;
  /** A named schema to link to. */
  ref?: string;
}

/** Merges allOf parts (and resolves refs) into one object schema. */
export function flattenSchema(doc: OpenAPIDoc, schema: Schema | undefined, depth = 0): Schema {
  const s = resolveRef(doc, schema) ?? {};
  if (!s.allOf || depth > 6) return s;
  const out: Schema = { ...s, allOf: undefined, properties: { ...(s.properties ?? {}) }, required: [...(s.required ?? [])] };
  for (const part of s.allOf) {
    const p = flattenSchema(doc, part, depth + 1);
    Object.assign(out.properties!, p.properties ?? {});
    out.required!.push(...(p.required ?? []));
    out.description ??= p.description;
    out.type ??= p.type;
    out.items ??= p.items;
  }
  return out;
}

/** The properties of an object schema as table rows, required ones first. */
export function propertyRows(doc: OpenAPIDoc, schema: Schema | undefined): PropertyRow[] {
  const s = flattenSchema(doc, schema);
  const required = new Set(s.required ?? []);
  const rows = Object.entries(s.properties ?? {}).map(([name, p]) => {
    const inner = p.allOf?.length === 1 && p.allOf[0]?.$ref ? p.allOf[0] : p;
    const ref = inner.$ref ?? (inner.items?.$ref && (Array.isArray(inner.type) ? inner.type.includes('array') : inner.type === 'array') ? inner.items.$ref : undefined);
    return {
      name,
      type: typeLabel(doc, inner),
      required: required.has(name),
      description: p.description ?? resolveRef(doc, inner)?.description ?? '',
      ref: ref ? refName(ref) : undefined,
    };
  });
  return rows.sort((a, b) => Number(b.required) - Number(a.required));
}

/** An example value for a schema: its own example, else one built from its type. */
export function exampleValue(doc: OpenAPIDoc, schema: Schema | undefined, depth = 0, seen: ReadonlySet<string> = new Set()): unknown {
  if (!schema) return null;
  if (schema.$ref) {
    if (seen.has(schema.$ref) || depth > 5) return {};
    return exampleValue(doc, resolveRef(doc, schema), depth + 1, new Set([...seen, schema.$ref]));
  }
  if (schema.example !== undefined) return schema.example;
  if (schema.examples?.length) return schema.examples[0];
  if (schema.default !== undefined) return schema.default;
  if (schema.const !== undefined) return schema.const;
  if (schema.enum?.length) return schema.enum[0];
  if (schema.allOf) return exampleValue(doc, flattenSchema(doc, schema), depth, seen);
  const choice = schema.oneOf ?? schema.anyOf;
  if (choice) {
    const nonNull = choice.find((s) => s.type !== 'null') ?? choice[0];
    return exampleValue(doc, nonNull, depth + 1, seen);
  }
  const types = Array.isArray(schema.type) ? schema.type.filter((t) => t !== 'null') : schema.type ? [schema.type] : [];
  const type = types[0] ?? (schema.properties ? 'object' : undefined);
  switch (type) {
    case 'string':
      if (schema.format === 'date-time') return '2026-01-31T00:00:00Z';
      if (schema.format === 'uri') return 'https://hangar.example.com/';
      if (schema.format === 'password') return '********';
      return 'string';
    case 'integer':
    case 'number':
      return schema.minimum ?? 0;
    case 'boolean':
      return false;
    case 'array':
      return depth > 5 ? [] : [exampleValue(doc, schema.items, depth + 1, seen)];
    case 'object': {
      if (depth > 5) return {};
      const out: Record<string, unknown> = {};
      for (const [name, p] of Object.entries(schema.properties ?? {})) {
        if (p.readOnly) continue;
        out[name] = exampleValue(doc, p, depth + 1, seen);
      }
      return out;
    }
    default:
      return null;
  }
}

/** Quotes a string for a POSIX shell. */
export function shellQuote(s: string): string {
  return /^[\w@%+=:,./-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;
}

/** The path with its parameters filled in; empty ones stay as `{name}`. */
export function fillPath(path: string, values: Record<string, string>): string {
  return path.replace(/\{([^}]+)\}/g, (m, name: string) => (values[name] ? encodeURIComponent(values[name]) : m));
}

/** The query string for the non-empty values, with a leading "?". */
export function queryString(values: Record<string, string>): string {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(values)) if (v !== '') qs.set(k, v);
  const s = qs.toString();
  return s ? `?${s}` : '';
}

export interface CurlOptions {
  /** The server's public URL, without a trailing slash. */
  base: string;
  pathValues?: Record<string, string>;
  queryValues?: Record<string, string>;
  /** The JSON body as text. */
  body?: string;
}

/**
 * A copyable curl command for op against base, authenticated with the token
 * in $HANGAR_TOKEN (left out for public operations). Unfilled path
 * parameters read `<name>`.
 */
export function curlCommand(op: Operation, o: CurlOptions): string {
  const values = Object.fromEntries(op.parameters.filter((p) => p.in === 'path').map((p) => [p.name, o.pathValues?.[p.name] || `<${p.name}>`]));
  const path = op.path.replace(/\{([^}]+)\}/g, (_, name: string) => (values[name]?.startsWith('<') ? values[name]! : encodeURIComponent(values[name] ?? '')));
  const url = `${o.base}/api/v1${path}${queryString(o.queryValues ?? {})}`;
  const parts = ['curl -fsS'];
  if (op.method !== 'GET') parts.push(`-X ${op.method}`);
  if (!op.public) parts.push('-H "Authorization: Bearer $HANGAR_TOKEN"');
  const body = o.body?.trim();
  if (body && op.body) parts.push(`-H 'Content-Type: ${op.body.contentType}'`);
  parts.push(`"${url.replace(/(["$`\\])/g, '\\$1')}"`);
  if (body && op.body) parts.push(`-d ${shellQuote(body)}`);
  return parts.join(' \\\n  ');
}

/** The example JSON body for an operation, pretty-printed; empty without a JSON body. */
export function exampleBody(doc: OpenAPIDoc, op: Operation): string {
  if (!op.body || !op.body.contentType.includes('json')) return '';
  return JSON.stringify(exampleValue(doc, op.body.schema), null, 2);
}

/** Named schemas, sorted by name. */
export function schemaNames(doc: OpenAPIDoc): string[] {
  return Object.keys(doc.components?.schemas ?? {}).sort((a, b) => a.localeCompare(b));
}

/** An HTML id for an operation or schema anchor. */
export function anchorId(kind: 'op' | 'tag' | 'schema', name: string): string {
  return `${kind}-${name.replace(/[^A-Za-z0-9_-]/g, '-')}`;
}
