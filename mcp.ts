import { text, type Forge } from './store.ts';
import type { Reply } from './rest.ts';

const revisions = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26'];
const serverInfo = { name: 'forge', version: '1.0.0' };
const capabilities = { tools: { listChanged: false } };

const tools = [
  tool('create_repo', 'Create a Git repository', ['name'], { name: 'string', description: 'string', private: 'boolean', auto_init: 'boolean' }),
  tool('get_repo', 'Read a repository by owner and name', ['owner', 'repo'], { owner: 'string', repo: 'string' }),
  tool('create_issue', 'Open an issue in a repository', ['owner', 'repo', 'title'], { owner: 'string', repo: 'string', title: 'string', body: 'string' }),
  tool('list_issues', 'List repository issues by state', ['owner', 'repo'], { owner: 'string', repo: 'string', state: 'string' }),
  tool('create_pull_request', 'Open a pull request between two branches', ['owner', 'repo', 'title', 'head', 'base'],
    { owner: 'string', repo: 'string', title: 'string', head: 'string', base: 'string', body: 'string' }),
  tool('merge_pull_request', 'Squash a pull request into its base branch', ['owner', 'repo', 'pull_number'],
    { owner: 'string', repo: 'string', pull_number: 'integer', sha: 'string', commit_title: 'string' }),
];

function tool(name: string, description: string, required: string[], fields: Record<string, string>) {
  const properties = Object.fromEntries(Object.entries(fields).map(([key, type]) => [key, { type }]));
  return { name, description, inputSchema: { type: 'object', properties, required } };
}

export function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

export function rpcError(code: number, message: string, id: unknown = null, status = 200): Reply {
  return { status, value: { jsonrpc: '2.0', id, error: { code, message } } };
}

export function mcp(forge: Forge, request: Record<string, unknown>): Reply {
  const { method } = request;
  const id = typeof request.id === 'number' || typeof request.id === 'string' ? request.id : null;
  if (request.jsonrpc !== '2.0' || typeof method !== 'string') return rpcError(-32600, 'Invalid Request', id, 400);
  if (id === null) return { status: 202 };
  const result = (value: unknown): Reply => ({ status: 200, value: { jsonrpc: '2.0', id, result: value } });
  const params = record(request.params);
  if (method === 'server/discover') return result({ supportedVersions: revisions, serverInfo, capabilities });
  if (method === 'initialize') {
    const version = text(params.protocolVersion);
    return result({ protocolVersion: revisions.includes(version) ? version : revisions[1], serverInfo, capabilities });
  }
  if (method === 'ping') return result({});
  if (method === 'tools/list') return result({ tools });
  if (method !== 'tools/call') return rpcError(-32601, 'Method not found', id);
  const definition = tools.find((item) => item.name === params.name);
  if (!definition) return rpcError(-32602, 'Unknown tool', id);
  const args = record(params.arguments);
  for (const key of definition.inputSchema.required) {
    const type = definition.inputSchema.properties[key].type;
    if (type === 'string' ? !text(args[key]).trim() : !Number.isSafeInteger(args[key])) {
      return rpcError(-32602, 'Missing or invalid argument: ' + key, id);
    }
  }
  try {
    const output = runTool(forge, definition.name, args);
    return result({ content: [{ type: 'text', text: JSON.stringify(output) }] });
  } catch (error) {
    return result({ isError: true, content: [{ type: 'text', text: error instanceof Error ? error.message : 'Tool failed' }] });
  }
}

function runTool(forge: Forge, name: string, args: Record<string, unknown>) {
  if (name === 'create_repo') return forge.repoJson(forge.createRepo(args));
  const repo = forge.getRepo(text(args.owner), text(args.repo));
  switch (name) {
    case 'get_repo': return forge.repoJson(repo);
    case 'create_issue': return forge.createIssue(repo, args);
    case 'list_issues': return forge.issues(repo, text(args.state) || 'open');
    case 'create_pull_request': return forge.createPull(repo, args);
    case 'merge_pull_request': return forge.mergePull(repo, forge.getPull(repo, Number(args.pull_number)), args);
    default: throw new Error('Unknown tool');
  }
}
