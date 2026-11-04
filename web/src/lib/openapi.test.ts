import { describe, expect, it } from 'vitest';
import { openAPIDoc as doc } from '../mocks/openapi';
import {
  curlCommand,
  exampleBody,
  fillPath,
  filterOperations,
  groupByTag,
  listOperations,
  propertyRows,
  queryString,
  shellQuote,
  typeLabel,
} from './openapi';

describe('OpenAPI reading', () => {
  const ops = listOperations(doc);
  const op = (id: string) => ops.find((o) => o.id === id)!;

  it('lists operations with merged and resolved parameters', () => {
    expect(ops.map((o) => `${o.method} ${o.path}`)).toEqual([
      'GET /auth/session',
      'GET /openapi.yaml',
      'GET /repositories',
      'POST /repositories',
      'GET /repositories/{name}',
      'POST /tokens/{id}/rotate',
      'GET /events',
    ]);
    expect(op('listRepositories').parameters.map((p) => `${p.in}:${p.name}`)).toEqual(['query:format', 'query:cursor', 'query:limit']);
    expect(op('getRepository').parameters[0]).toMatchObject({ name: 'name', in: 'path', required: true });
    expect(op('rotateToken').parameters[0]).toMatchObject({ name: 'id', in: 'path' });
    expect(op('getOpenAPIYAML').public).toBe(true);
    expect(op('getSession').public).toBe(false);
    expect(op('createRepository').responses.map((r) => r.status)).toEqual(['201', '400', '401', '403']);
    expect(op('createRepository').responses[1]).toMatchObject({ description: 'Invalid input', contentType: 'application/json' });
  });

  it('groups by tag in document order and searches', () => {
    expect(groupByTag(doc, ops).map((g) => [g.tag, g.operations.length])).toEqual([
      ['auth', 1],
      ['docs', 1],
      ['repositories', 3],
      ['access', 1],
      ['events', 1],
    ]);
    expect(filterOperations(ops, 'rotate').map((o) => o.id)).toEqual(['rotateToken']);
    expect(filterOperations(ops, 'post repositories').map((o) => o.id)).toEqual(['createRepository']);
    expect(filterOperations(ops, '  ')).toHaveLength(ops.length);
    expect(filterOperations(ops, 'nothing-like-this')).toEqual([]);
  });

  it('labels types and lists properties, required first', () => {
    expect(typeLabel(doc, { $ref: '#/components/schemas/Token' })).toBe('Token');
    expect(typeLabel(doc, { type: 'array', items: { $ref: '#/components/schemas/Repository' } })).toBe('Repository[]');
    expect(typeLabel(doc, { type: ['string', 'null'] })).toBe('string | null');
    expect(typeLabel(doc, { type: 'string', enum: ['a', 'b'] })).toBe('"a" | "b"');
    expect(typeLabel(doc, { type: 'string', format: 'date-time' })).toBe('string (date-time)');
    const rows = propertyRows(doc, { $ref: '#/components/schemas/Repository' });
    expect(rows.filter((r) => r.required).map((r) => r.name)).toEqual(['name', 'format', 'type', 'id', 'url']);
    expect(rows.find((r) => r.name === 'online')).toMatchObject({ required: false, type: 'boolean', description: 'Optional; true when omitted' });
    expect(propertyRows(doc, { $ref: '#/components/schemas/RepositoryList' }).find((r) => r.name === 'items')).toMatchObject({
      type: 'Repository[]',
      ref: 'Repository',
    });
  });

  it('builds example bodies and curl commands for this server', () => {
    expect(JSON.parse(exampleBody(doc, op('createRepository')))).toEqual({ name: 'npm-team', format: 'npm', type: 'hosted', online: false });
    expect(exampleBody(doc, op('getSession'))).toBe('');
    expect(curlCommand(op('getRepository'), { base: 'https://hangar.example.com' })).toBe(
      'curl -fsS \\\n  -H "Authorization: Bearer $HANGAR_TOKEN" \\\n  "https://hangar.example.com/api/v1/repositories/<name>"',
    );
    const post = curlCommand(op('createRepository'), { base: 'https://h.example', body: `{"name": "it's"}` });
    expect(post).toContain('-X POST');
    expect(post).toContain("-H 'Content-Type: application/json'");
    expect(post).toContain(`-d '{"name": "it'\\''s"}'`);
    expect(curlCommand(op('getOpenAPIYAML'), { base: 'https://h.example' })).not.toContain('Authorization');
    expect(curlCommand(op('listRepositories'), { base: 'https://h.example', queryValues: { format: 'npm', limit: '', cursor: '' } })).toContain(
      '"https://h.example/api/v1/repositories?format=npm"',
    );
    expect(curlCommand(op('getRepository'), { base: 'https://h.example', pathValues: { name: 'a b' } })).toContain('/repositories/a%20b"');
  });

  it('fills paths and quotes for the shell', () => {
    expect(fillPath('/tokens/{id}/rotate', { id: 't/1' })).toBe('/tokens/t%2F1/rotate');
    expect(fillPath('/tokens/{id}', {})).toBe('/tokens/{id}');
    expect(queryString({ a: '1', b: '' })).toBe('?a=1');
    expect(queryString({})).toBe('');
    expect(shellQuote('plain-value')).toBe('plain-value');
    expect(shellQuote('two words')).toBe("'two words'");
  });
});
