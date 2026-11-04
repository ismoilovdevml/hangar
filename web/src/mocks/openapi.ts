/**
 * A small OpenAPI document in the shape of api/openapi.yaml, served at
 * /api/v1/openapi.json in mock mode and tests. The real server publishes the
 * full document.
 */
import { http, HttpResponse } from 'msw';
import type { OpenAPIDoc } from '../lib/openapi';

const ref = (name: string) => ({ $ref: `#/components/schemas/${name}` });
const json = (name: string) => ({ content: { 'application/json': { schema: ref(name) } } });
const errors = {
  '401': { $ref: '#/components/responses/Unauthorized' },
  '403': { $ref: '#/components/responses/Forbidden' },
};

export const openAPIDoc: OpenAPIDoc = {
  openapi: '3.1.0',
  info: { title: 'Hangar API', version: '0.1.0', description: 'JSON REST API served under `/api/v1`.' },
  tags: [
    { name: 'auth', description: 'Sign-in, the current session and what the caller may do.' },
    { name: 'docs', description: 'This document and the guide for automated clients; public.' },
    { name: 'repositories', description: 'Hosted, proxy and group repositories.' },
    { name: 'access', description: 'Users, service accounts, roles and access tokens.' },
    { name: 'events', description: 'Server-sent events for live updates.' },
  ],
  paths: {
    '/auth/session': {
      get: {
        tags: ['auth'],
        operationId: 'getSession',
        summary: 'Current user and organization',
        responses: { '200': { description: 'Active session', ...json('Session') }, '401': errors['401'] },
      },
    },
    '/openapi.yaml': {
      get: {
        tags: ['docs'],
        operationId: 'getOpenAPIYAML',
        summary: 'This OpenAPI document as YAML',
        security: [],
        responses: { '200': { description: 'OpenAPI 3.1 document', content: { 'application/yaml': { schema: { type: 'string' } } } } },
      },
    },
    '/repositories': {
      get: {
        tags: ['repositories'],
        operationId: 'listRepositories',
        summary: 'List repositories the caller can read',
        parameters: [
          { name: 'format', in: 'query', schema: { type: 'string', enum: ['docker', 'npm', 'maven', 'nuget'] } },
          { $ref: '#/components/parameters/Cursor' },
          { $ref: '#/components/parameters/Limit' },
        ],
        responses: { '200': { description: 'Repositories', ...json('RepositoryList') }, '401': errors['401'] },
      },
      post: {
        tags: ['repositories'],
        operationId: 'createRepository',
        summary: 'Create a repository',
        description: 'Needs `system:repos:admin`.',
        requestBody: { required: true, ...json('RepositoryInput') },
        responses: { '201': { description: 'Created', ...json('Repository') }, '400': { $ref: '#/components/responses/BadRequest' }, ...errors },
      },
    },
    '/repositories/{name}': {
      parameters: [{ name: 'name', in: 'path', required: true, schema: { type: 'string' } }],
      get: {
        tags: ['repositories'],
        operationId: 'getRepository',
        summary: 'One repository',
        responses: { '200': { description: 'The repository', ...json('Repository') }, '404': { $ref: '#/components/responses/NotFound' } },
      },
    },
    '/tokens/{id}/rotate': {
      parameters: [{ $ref: '#/components/parameters/Id' }],
      post: {
        tags: ['access'],
        operationId: 'rotateToken',
        summary: 'Replace a token by a new one with the same scopes',
        requestBody: { required: false, ...json('TokenRotateInput') },
        responses: { '201': { description: 'The replacement', ...json('Token') }, ...errors },
      },
    },
    '/events': {
      get: {
        tags: ['events'],
        operationId: 'streamEvents',
        summary: 'Live updates',
        responses: { '200': { description: 'Event stream', content: { 'text/event-stream': { schema: { type: 'string' } } } } },
      },
    },
  },
  components: {
    parameters: {
      Cursor: { name: 'cursor', in: 'query', description: "Opaque cursor from the previous page's `next_cursor`", schema: { type: 'string' } },
      Limit: { name: 'limit', in: 'query', schema: { type: 'integer', minimum: 1, maximum: 1000 } },
      Id: { name: 'id', in: 'path', required: true, schema: { type: 'string' } },
    },
    responses: {
      BadRequest: { description: 'Invalid input', ...json('Error') },
      Unauthorized: { description: 'Not signed in, or credentials invalid', ...json('Error') },
      Forbidden: { description: 'Missing privilege', ...json('Error') },
      NotFound: { description: 'No such resource', ...json('Error') },
    },
    schemas: {
      Error: {
        type: 'object',
        required: ['code', 'message'],
        properties: { code: { type: 'string', description: 'Machine-readable code' }, message: { type: 'string' }, retry_after: { type: 'integer' } },
      },
      RepositoryInput: {
        type: 'object',
        required: ['name', 'format', 'type'],
        properties: {
          name: { type: 'string', pattern: '^[a-z0-9][a-z0-9-]{1,62}$', examples: ['npm-team'] },
          format: { type: 'string', enum: ['npm', 'docker', 'maven', 'nuget'] },
          type: { type: 'string', enum: ['hosted', 'proxy', 'group'] },
          online: { type: 'boolean', description: 'Optional; true when omitted' },
        },
      },
      Repository: {
        allOf: [ref('RepositoryInput'), { type: 'object', required: ['id', 'url'], properties: { id: { type: 'string' }, url: { type: 'string' } } }],
      },
      RepositoryList: {
        type: 'object',
        required: ['items'],
        properties: { items: { type: 'array', items: ref('Repository') }, next_cursor: { type: ['string', 'null'] } },
      },
      Session: {
        type: 'object',
        required: ['user'],
        properties: { user: { type: 'object', properties: { username: { type: 'string' } } }, privileges: { type: 'array', items: { type: 'string' } } },
      },
      Token: {
        type: 'object',
        required: ['id', 'name'],
        properties: { id: { type: 'string' }, name: { type: 'string' }, expires_at: { type: 'string', format: 'date-time' } },
      },
      TokenRotateInput: {
        type: 'object',
        properties: { revoke_old_after: { type: ['string', 'null'], description: 'Go duration; `0s` revokes the old token now', examples: ['24h'] } },
      },
    },
  },
};

export const openAPIHandlers = [http.get('*/api/v1/openapi.json', () => HttpResponse.json(openAPIDoc))];
