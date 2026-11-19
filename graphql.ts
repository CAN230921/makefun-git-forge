import { ApiError, globalId, missing, text, type Forge, type Issue, type Pull, type Repository } from './store.ts';
import { record } from './mcp.ts';

type Dictionary<T = unknown> = Record<string, T>;
type Value = (vars: Dictionary) => unknown;
type Directive = { name: string; args: Dictionary<Value> };
type Selection = { name: string; alias: string; args: Dictionary<Value>; children: Selection[]; directives: Directive[];
  on?: string; fragment?: string };
type Operation = { kind: 'query' | 'mutation'; name?: string; variables: Dictionary<Value>; selections: Selection[] };
type Fragment = { on: string; selections: Selection[] };
type Node = { type: string; fields: Dictionary };
type Resolver = (args: Dictionary) => unknown;

const node = (type: string, fields: Dictionary): Node => ({ type, fields });
const dictionary = <T>() => Object.create(null) as Dictionary<T>;
const literals = Object.assign(Object.create(null), { true: true, false: false, null: null });

class Parser {
  private tokens: string[] = [];
  private offset = 0;
  private depth = 0;
  fragments = new Map<string, Fragment>();

  constructor(source: string) {
    const token = /(?:#[^\r\n]*|[\s,]+)|\.\.\.|[_A-Za-z][_A-Za-z\d]*|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?|"(?:[^"\\\r\n]|\\.)*"|[!$():=@\[\]{|}]/gy;
    let match: RegExpExecArray | null;
    let offset = 0;
    while ((match = token.exec(source))) {
      const value = match[0];
      offset = token.lastIndex;
      if (!/^[#\s,]/.test(value)) this.tokens.push(value);
      if (this.tokens.length > 16000) throw new Error('Query has too many tokens');
    }
    if (offset !== source.length) throw new Error('Invalid GraphQL token near offset ' + offset);
  }

  peek() { return this.tokens[this.offset]; }

  next() {
    const token = this.tokens[this.offset++];
    if (token === undefined) throw new Error('Unexpected end of query');
    return token;
  }

  take(token: string) {
    if (this.peek() !== token) return false;
    this.offset++;
    return true;
  }

  expect(token: string) { if (!this.take(token)) throw new Error('Expected ' + token + ' in query'); }

  name() {
    const value = this.next();
    if (!/^[_a-zA-Z][_a-zA-Z0-9]*$/.test(value)) throw new Error('Expected a GraphQL name');
    return value;
  }

  value(): Value {
    if (++this.depth > 60) throw new Error('Query is too deeply nested');
    try {
      if (this.take('$')) {
        const name = this.name();
        return (vars) => Object.hasOwn(vars, name) ? vars[name] : undefined;
      }
      if (this.take('[')) {
        const values: Value[] = [];
        while (!this.take(']')) values.push(this.value());
        return (vars) => values.map((value) => value(vars));
      }
      if (this.take('{')) {
        const entries = this.argumentsUntil('}');
        return (vars) => evaluateArguments(entries, vars);
      }
      const token = this.next();
      if (token.startsWith('"')) { const value = JSON.parse(token); return () => value; }
      if (/^-?\d/.test(token)) { const value = Number(token); return () => value; }
      if (!/^[_a-zA-Z]/.test(token)) throw new Error('Invalid GraphQL value');
      return () => Object.hasOwn(literals, token) ? literals[token] : token;
    } finally {
      this.depth--;
    }
  }

  argumentsUntil(close: ')' | '}') {
    const args = dictionary<Value>();
    while (!this.take(close)) {
      const name = this.name();
      this.expect(':');
      args[name] = this.value();
    }
    return args;
  }

  directives() {
    const directives: Directive[] = [];
    while (this.take('@')) {
      const name = this.name();
      directives.push({ name, args: this.take('(') ? this.argumentsUntil(')') : dictionary<Value>() });
    }
    return directives;
  }

  selectionSet(): Selection[] {
    this.expect('{');
    if (++this.depth > 60) throw new Error('Query is too deeply nested');
    const selections: Selection[] = [];
    while (!this.take('}')) {
      if (this.take('...')) {
        const on = this.take('on') ? this.name() : undefined;
        const fragment = !on && this.peek() !== '@' && this.peek() !== '{' ? this.name() : undefined;
        const directives = this.directives();
        selections.push({ name: '', alias: '', args: dictionary(), on, fragment, children: fragment ? [] : this.selectionSet(), directives });
      } else {
        const alias = this.name();
        const name = this.take(':') ? this.name() : alias;
        const args = this.take('(') ? this.argumentsUntil(')') : dictionary<Value>();
        const directives = this.directives();
        selections.push({ name, alias, args, directives, children: this.peek() === '{' ? this.selectionSet() : [] });
      }
    }
    this.depth--;
    if (!selections.length) throw new Error('Selection set is empty');
    return selections;
  }

  variableType(depth = 0): void {
    if (depth > 60) throw new Error('Variable type is too deeply nested');
    if (this.take('[')) {
      this.variableType(depth + 1);
      this.expect(']');
    } else {
      this.name();
    }
    this.take('!');
  }

  parse() {
    const operations: Operation[] = [];
    while (this.peek() !== undefined) {
      if (this.take('fragment')) {
        const name = this.name();
        this.expect('on');
        const on = this.name();
        this.directives();
        this.fragments.set(name, { on, selections: this.selectionSet() });
        continue;
      }
      const kind = this.peek() === '{' ? 'query' : this.name();
      if (kind !== 'query' && kind !== 'mutation') throw new Error('Unsupported GraphQL operation ' + kind);
      const name = /^[_a-zA-Z]/.test(this.peek() ?? '') ? this.name() : undefined;
      const variables = dictionary<Value>();
      if (this.take('(')) {
        while (!this.take(')')) {
          this.expect('$');
          const key = this.name();
          this.expect(':');
          this.variableType();
          if (this.take('=')) variables[key] = this.value();
          this.directives();
        }
      }
      this.directives();
      operations.push({ kind, name, variables, selections: this.selectionSet() });
    }
    return operations;
  }
}

const evaluateArguments = (args: Dictionary<Value>, vars: Dictionary) =>
  Object.fromEntries(Object.entries(args).map(([key, parse]) => [key, parse(vars)]));

function project(value: unknown, selections: Selection[], vars: Dictionary, fragments: Map<string, Fragment>,
  visiting: Set<string>, limit: { count: number }): unknown {
  if (value == null) return null;
  if (Array.isArray(value)) {
    if (!selections.length) return value;
    return value.map((item) => project(item, selections, vars, fragments, visiting, limit));
  }
  if (typeof value !== 'object') {
    if (selections.length) throw new Error('A scalar cannot have a selection set');
    return value;
  }
  const current = value as Node;
  if (!selections.length) throw new Error('Field of type ' + current.type + ' requires a selection set');
  const result = dictionary();
  for (const field of selections) {
    if (++limit.count > 50000) throw new Error('Query exceeds the result field budget');
    if (field.directives.some(({ name, args }) => name === 'skip' && args.if?.(vars) === true
      || name === 'include' && args.if?.(vars) === false)) continue;
    if (!field.name) {
      const fragment = field.fragment ? fragments.get(field.fragment) : undefined;
      if (field.fragment && !fragment) throw new Error('Unknown fragment ' + field.fragment);
      const on = fragment?.on ?? field.on;
      if (on && on !== current.type && !(on === 'Node' && Object.hasOwn(current.fields, 'id'))) continue;
      if (field.fragment) {
        if (visiting.has(field.fragment)) throw new Error('Cyclic fragment ' + field.fragment);
        visiting.add(field.fragment);
      }
      Object.assign(result, project(current, fragment?.selections ?? field.children, vars, fragments, visiting, limit));
      if (field.fragment) visiting.delete(field.fragment);
      continue;
    }
    if (field.name === '__typename') {
      result[field.alias] = current.type;
      continue;
    }
    if (!Object.hasOwn(current.fields, field.name)) throw new Error('Cannot query field "' + field.name + '" on type "' + current.type + '"');
    const child = current.fields[field.name];
    const resolved = typeof child === 'function' ? (child as Resolver)(evaluateArguments(field.args, vars)) : child;
    result[field.alias] = project(resolved, field.children, vars, fragments, visiting, limit);
  }
  return result;
}

function connection(type: string, items: Node[], args: Dictionary) {
  const first = Math.min(100, Math.max(0, Number.isSafeInteger(args.first) ? Number(args.first) : 30));
  const cursor = (offset: number) => Buffer.from('cursor:' + offset).toString('base64url');
  const marker = typeof args.after === 'string' ? Buffer.from(args.after, 'base64url').toString() : '';
  const start = /^cursor:\d+$/.test(marker) ? Number(marker.slice(7)) + 1 : 0;
  const end = Math.min(start + first, items.length);
  const nodes = items.slice(start, end);
  const edges = nodes.map((item, index) => node(type + 'Edge', { node: item, cursor: cursor(start + index) }));
  return node(type + 'Connection', {
    totalCount: items.length,
    nodes,
    edges,
    pageInfo: node('PageInfo', {
      hasNextPage: end < items.length, hasPreviousPage: start > 0,
      endCursor: nodes.length ? cursor(end - 1) : null, startCursor: nodes.length ? cursor(start) : null,
    }),
  });
}

const schemaFields: Record<string, string[]> = {
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'description', 'isPrivate', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  Issue: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'author', 'createdAt', 'updatedAt', 'closedAt', 'comments', 'labels'],
  PullRequest: ['id', 'databaseId', 'number', 'title', 'body', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'mergeable', 'state', 'merged', 'mergedAt', 'files'],
  User: ['id', 'login'],
  Query: ['viewer', 'repository', 'node', 'search'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
};

function introspection(name: string) {
  const enumValues = name === 'SearchType' ? ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] : undefined;
  const fields = Object.hasOwn(schemaFields, name) ? schemaFields[name] : undefined;
  if (!fields && !enumValues) return null;
  return node('__Type', {
    name, kind: enumValues ? 'ENUM' : 'OBJECT', description: null,
    fields: fields?.map((name) => node('__Field', { name, description: null, isDeprecated: false })) ?? null,
    enumValues: enumValues?.map((name) => node('__EnumValue', { name, description: null, isDeprecated: false })) ?? null,
  });
}

const viewer = (login: string) => node('User', { id: globalId('User', 1), login });
const commentNode = (comment: { node_id: string; body: string; created_at: string; user: { login: string } }) =>
  node('IssueComment', { id: comment.node_id, body: comment.body, createdAt: comment.created_at, author: viewer(comment.user.login) });

const issueNode = (repo: Repository, issue: Issue) => node('Issue', {
  id: issue.node_id, databaseId: issue.id, number: issue.number, title: issue.title, body: issue.body, state: issue.state.toUpperCase(),
  author: viewer(issue.user.login), createdAt: issue.created_at, updatedAt: issue.updated_at, closedAt: issue.closed_at,
  comments: (args: Dictionary) => connection('IssueComment', (repo.comments.get(issue.number) ?? []).map(commentNode), args),
  labels: (args: Dictionary) => connection('Label', issue.labels.map((label) => node('Label', label)), args),
});

const pullState = (pull: Pull) => pull.merged ? 'MERGED' : pull.state.toUpperCase();
const pullNode = (repo: Repository, pull: Pull) => node('PullRequest', {
  id: pull.node_id, databaseId: pull.id, number: pull.number, title: pull.title, body: pull.body,
  headRefName: pull.head.ref, baseRefName: pull.base.ref, headRefOid: pull.head.sha, baseRefOid: pull.base.sha,
  mergeable: () => repo.mergedFiles(pull) === null ? 'CONFLICTING' : 'MERGEABLE', state: pullState(pull),
  merged: pull.merged, mergedAt: pull.merged_at,
  files: (args: Dictionary) => connection('PullRequestChangedFile', repo.filesForPull(pull).map((file) => node('PullRequestChangedFile', {
    path: file.filename, additions: file.additions, deletions: file.deletions, changeType: file.status.toUpperCase(),
  })), args),
});

const repoNode = (forge: Forge, repo: Repository) => node('Repository', {
  id: globalId('Repository', repo.id), databaseId: repo.id, name: repo.name, nameWithOwner: forge.owner + '/' + repo.name,
  owner: viewer(forge.owner), description: repo.description, isPrivate: repo.private, viewerPermission: 'ADMIN', hasIssuesEnabled: true,
  defaultBranchRef: repo.refs.has(repo.head) ? node('Ref', { name: repo.defaultBranch }) : null,
  issue: ({ number }: Dictionary) => repo.issues.has(Number(number)) ? issueNode(repo, forge.getIssue(repo, Number(number))) : null,
  issues: (args: Dictionary) => {
    const states = Array.isArray(args.states) ? args.states : [];
    const order = record(args.orderBy);
    const ascending = order.direction !== 'DESC';
    const items = forge.issues(repo, 'all', ascending).filter((issue) => !states.length || states.includes(issue.state.toUpperCase()));
    return connection('Issue', items.map((issue) => issueNode(repo, issue)), args);
  },
  pullRequest: ({ number }: Dictionary) => repo.pulls.has(Number(number)) ? pullNode(repo, forge.getPull(repo, Number(number))) : null,
  pullRequests: (args: Dictionary) => {
    const states = Array.isArray(args.states) ? args.states : [];
    const pulls = [...repo.pulls.values()].filter((pull) => !states.length || states.includes(pullState(pull)));
    return connection('PullRequest', pulls.map((pull) => pullNode(repo, pull)), args);
  },
});

function lookup(forge: Forge, id: unknown) {
  return forge.nodes.get(text(id));
}

function mutation(forge: Forge) {
  return node('Mutation', {
    createIssue: ({ input }: Dictionary) => {
      const args = record(input);
      const ref = lookup(forge, args.repositoryId);
      if (ref?.type !== 'Repository') return missing();
      const issue = forge.createIssue(ref.repo, args);
      return node('CreateIssuePayload', { issue: issueNode(ref.repo, issue), clientMutationId: args.clientMutationId ?? null });
    },
    closeIssue: ({ input }: Dictionary) => {
      const args = record(input);
      const ref = lookup(forge, args.issueId);
      if (ref?.type !== 'Issue') return missing();
      const issue = forge.updateIssue(forge.getIssue(ref.repo, ref.number!), { state: 'closed' });
      return node('CloseIssuePayload', { issue: issueNode(ref.repo, issue), clientMutationId: args.clientMutationId ?? null });
    },
    createPullRequest: ({ input }: Dictionary) => {
      const args = record(input);
      const ref = lookup(forge, args.repositoryId);
      if (ref?.type !== 'Repository') return missing();
      const pull = forge.createPull(ref.repo, { ...args, head: args.headRefName, base: args.baseRefName });
      return node('CreatePullRequestPayload', { pullRequest: pullNode(ref.repo, pull), clientMutationId: args.clientMutationId ?? null });
    },
    mergePullRequest: ({ input }: Dictionary) => {
      const args = record(input);
      const ref = lookup(forge, args.pullRequestId);
      if (ref?.type !== 'PullRequest') return missing();
      const pull = forge.getPull(ref.repo, ref.number!);
      forge.mergePull(ref.repo, pull, { sha: args.expectedHeadOid, commit_title: args.commitHeadline });
      return node('MergePullRequestPayload', { pullRequest: pullNode(ref.repo, pull), clientMutationId: args.clientMutationId ?? null });
    },
    addComment: ({ input }: Dictionary) => {
      const args = record(input);
      const ref = lookup(forge, args.subjectId);
      if (ref?.type !== 'Issue') return missing();
      const comment = forge.addComment(ref.repo, ref.number!, args);
      const commentEdge = node('IssueCommentEdge', { node: commentNode(comment) });
      return node('AddCommentPayload', { commentEdge, clientMutationId: args.clientMutationId ?? null });
    },
  });
}

function query(forge: Forge) {
  return node('Query', {
    viewer: viewer(forge.owner),
    repository: ({ owner, name }: Dictionary) => {
      try {
        return repoNode(forge, forge.getRepo(text(owner), text(name)));
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return null;
        throw error;
      }
    },
    node: ({ id }: Dictionary) => {
      const ref = lookup(forge, id);
      if (ref?.type === 'Repository') return repoNode(forge, ref.repo);
      if (ref?.type === 'Issue') return issueNode(ref.repo, forge.getIssue(ref.repo, ref.number!));
      if (ref?.type === 'PullRequest') return pullNode(ref.repo, forge.getPull(ref.repo, ref.number!));
      return null;
    },
    search: (args: Dictionary) => {
      const term = text(args.query).toLowerCase();
      const results: Node[] = [];
      for (const repo of forge.repos.values()) {
        if (args.type === 'REPOSITORY' && repo.name.toLowerCase().includes(term)) results.push(repoNode(forge, repo));
        if (args.type === 'ISSUE') {
          for (const issue of repo.issues.values()) {
            if ((issue.title + ' ' + issue.body).toLowerCase().includes(term)) results.push(issueNode(repo, issue));
          }
        }
      }
      const result = connection('SearchResultItem', results, args);
      result.fields.issueCount = args.type === 'ISSUE' ? results.length : 0;
      result.fields.repositoryCount = args.type === 'REPOSITORY' ? results.length : 0;
      return result;
    },
    __type: ({ name }: Dictionary) => introspection(text(name)),
  });
}

export function graphql(forge: Forge, body: unknown): unknown {
  if (Array.isArray(body)) return body.map((item) => execute(forge, item));
  return execute(forge, body);
}

function execute(forge: Forge, value: unknown) {
  const body = record(value);
  try {
    const source = text(body.query);
    if (!source || source.length > 512 * 1024) throw new Error('Expected a GraphQL query');
    const parser = new Parser(source);
    const operations = parser.parse();
    const name = text(body.operationName);
    const operation = name ? operations.find((op) => op.name === name) : operations.length === 1 ? operations[0] : undefined;
    if (!operation) throw new Error('Select a named GraphQL operation');
    const provided = record(body.variables);
    const variables = { ...evaluateArguments(operation.variables, provided), ...provided };
    const root = operation.kind === 'mutation' ? mutation(forge) : query(forge);
    return { data: project(root, operation.selections, variables, parser.fragments, new Set(), { count: 0 }) };
  } catch (error) {
    return { errors: [{ message: error instanceof Error ? error.message : 'GraphQL operation failed' }] };
  }
}
