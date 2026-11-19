import { isOid, ObjectStore, packObjects, readCommit, readTree, unpack, ZERO, type GitObject } from './objects.ts';

const flush = Buffer.from('0000');
const delimiter = Buffer.from('0001');
const packet = (data: Buffer | string) => {
  const bytes = Buffer.isBuffer(data) ? data : Buffer.from(data);
  return Buffer.concat([Buffer.from((bytes.length + 4).toString(16).padStart(4, '0')), bytes]);
};
const packets = (lines: string[]) => lines.map(packet);

function readPackets(body: Buffer, stopAtFlush = false) {
  const lines: string[] = [];
  let offset = 0;
  while (offset + 4 <= body.length) {
    const header = body.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-f]{4}$/i.test(header)) throw new Error('invalid pkt-line');
    const size = parseInt(header, 16);
    if (size === 3 || size === 4 || size > 65520 || offset + size > body.length) throw new Error('invalid pkt-line length');
    if (size >= 5) lines.push(body.toString('utf8', offset + 4, offset + size).replace(/\n$/, ''));
    offset += Math.max(4, size);
    if (!size && stopAtFlush) return { lines, offset };
  }
  if (offset !== body.length || stopAtFlush) throw new Error('incomplete pkt-line stream');
  return { lines, offset };
}

function sideband(data: Buffer): Buffer[] {
  const frames: Buffer[] = [];
  for (let offset = 0; offset < data.length; offset += 65515) {
    const chunk = data.subarray(offset, offset + 65515);
    const frame = Buffer.allocUnsafe(chunk.length + 5);
    frame.write((chunk.length + 5).toString(16).padStart(4, '0'), 0, 4, 'ascii');
    frame[4] = 1;
    chunk.copy(frame, 5);
    frames.push(frame);
  }
  return frames;
}

export function validRef(ref: string): boolean {
  return ref.length <= 255 && /^refs\/(heads|tags)\//.test(ref)
    && !/[\x00-\x20\x7f~^:?*\[\\]/.test(ref)
    && !ref.includes('..') && !ref.includes('@{') && !ref.endsWith('.')
    && ref.split('/').every((part) => part && !part.startsWith('.') && !part.endsWith('.lock'));
}

const tagTarget = (object: GitObject) => {
  const header = object.bytes.toString('ascii', 0, 48);
  const target = header.startsWith('object ') ? header.slice(7, 47) : '';
  if (!isOid(target)) throw new Error('invalid tag');
  return target;
};

export class GitRepository {
  store = new ObjectStore();
  refs = new Map<string, string>();
  defaultBranch: string;
  cachedFetch?: { key: string; body: Buffer };

  constructor(defaultBranch = 'main') {
    this.defaultBranch = defaultBranch;
  }

  get head() { return 'refs/heads/' + this.defaultBranch; }

  resolve(ref = this.defaultBranch): string | undefined {
    return this.refs.get(ref) ?? this.refs.get('refs/heads/' + ref)
      ?? this.refs.get('refs/tags/' + ref) ?? (this.store.objects.has(ref) ? ref : undefined);
  }

  peel(oid: string): string {
    const seen = new Set<string>();
    let object = this.store.objects.get(oid);
    while (object?.kind === 'tag' && !seen.has(oid)) {
      seen.add(oid);
      oid = tagTarget(object);
      object = this.store.objects.get(oid);
    }
    return oid;
  }

  parents(oid: string): string[] {
    return this.store.commit(oid)?.parents ?? [];
  }

  isAncestor(ancestor: string, tip: string, extra: Map<string, GitObject>): boolean {
    const pending = [tip];
    const seen = new Set<string>();
    while (pending.length) {
      const oid = pending.pop()!;
      if (oid === ancestor) return true;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = extra.get(oid);
      pending.push(...(object?.kind === 'commit' ? readCommit(object.bytes).parents : this.parents(oid)));
    }
    return false;
  }

  mergeBase(left: string, right: string): string | undefined {
    const ancestors = new Set<string>();
    const pending = [left];
    while (pending.length) {
      const oid = pending.pop()!;
      if (ancestors.has(oid)) continue;
      ancestors.add(oid);
      pending.push(...this.parents(oid));
    }
    const seen = new Set<string>();
    const queue = [right];
    for (let i = 0; i < queue.length; i++) {
      const oid = queue[i];
      if (ancestors.has(oid)) return oid;
      if (seen.has(oid)) continue;
      seen.add(oid);
      queue.push(...this.parents(oid));
    }
  }

  advertise(service: string, v2: boolean): Buffer {
    if (v2 && service === 'git-upload-pack') {
      return Buffer.concat([...packets(['version 2\n', 'agent=forge/1.0\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n']), flush]);
    }
    const write = service === 'git-receive-pack';
    const caps = write
      ? 'report-status delete-refs side-band-64k ofs-delta object-format=sha1 agent=forge/1.0'
      : 'side-band-64k ofs-delta shallow object-format=sha1 agent=forge/1.0 symref=HEAD:' + this.head;
    const lines = [...this.refs].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0);
    const head = this.refs.get(this.head);
    if (head && !write) lines.unshift(['HEAD', head]);
    if (!lines.length && write) lines.push(['capabilities^{}', ZERO]);
    const output = lines.flatMap(([name, oid], index) => {
      const line = oid + ' ' + name + (index === 0 ? '\0' + caps : '') + '\n';
      const peeled = this.peel(oid);
      return name.startsWith('refs/tags/') && peeled !== oid && !write
        ? [line, peeled + ' ' + name + '^{}\n'] : [line];
    });
    return Buffer.concat([packet('# service=' + service + '\n'), flush, ...packets(output), flush]);
  }

  listRefs(args: string[]): Buffer {
    const prefixes = args.filter((line) => line.startsWith('ref-prefix ')).map((line) => line.slice(11));
    const matches = (name: string) => !prefixes.length || prefixes.some((prefix) => name.startsWith(prefix));
    const result: string[] = [];
    if (matches('HEAD')) {
      const head = this.refs.get(this.head);
      const suffix = args.includes('symrefs') || !head ? ' symref-target:' + this.head : '';
      if (head || args.includes('unborn')) result.push((head ?? 'unborn') + ' HEAD' + suffix + '\n');
    }
    for (const name of [...this.refs.keys()].sort()) {
      if (!matches(name)) continue;
      const oid = this.refs.get(name)!;
      const peeled = args.includes('peel') ? this.peel(oid) : oid;
      result.push(oid + ' ' + name + (peeled !== oid ? ' peeled:' + peeled : '') + '\n');
    }
    return Buffer.concat([...packets(result), flush]);
  }

  reach(roots: string[], depth: number, boundaries: Set<string>, relative = false) {
    const selected = new Set<string>();
    const shallow = new Set<string>();
    const budgets = new Map<string, number>();
    const commits = roots.map((oid) => ({ oid, remaining: relative ? Infinity : depth }));
    const addTree = (root: string) => {
      const pending = [root];
      while (pending.length) {
        const oid = pending.pop()!;
        if (selected.has(oid)) continue;
        const object = this.store.objects.get(oid);
        if (!object) continue;
        selected.add(oid);
        if (object.kind === 'tree') {
          for (const entry of this.store.tree(oid)!) if (entry.mode !== '160000') pending.push(entry.oid);
        }
      }
    };
    for (let index = 0; index < commits.length; index++) {
      const next = commits[index];
      const oid = next.oid;
      const object = this.store.objects.get(oid);
      if (!object || (budgets.get(oid) ?? 0) >= next.remaining) continue;
      budgets.set(oid, next.remaining);
      if (object.kind === 'tag') {
        selected.add(oid);
        commits.push({ oid: tagTarget(object), remaining: next.remaining });
        continue;
      }
      const commit = this.store.commit(oid);
      if (!commit) {
        addTree(oid);
        continue;
      }
      selected.add(oid);
      addTree(commit.tree);
      const remaining = relative && boundaries.has(oid) ? Math.min(next.remaining, depth + 1) : next.remaining;
      if ((!relative && boundaries.has(oid)) || remaining <= 1) {
        if (commit.parents.length) shallow.add(oid);
      } else {
        shallow.delete(oid);
        for (const parent of commit.parents) commits.push({ oid: parent, remaining: remaining - 1 });
      }
    }
    for (const oid of shallow) {
      if (this.parents(oid).every((parent) => selected.has(parent))) shallow.delete(oid);
    }
    return { selected, shallow };
  }

  upload(body: Buffer, v2: boolean): Buffer {
    const { lines } = readPackets(body);
    if (v2 && lines.includes('command=ls-refs')) return this.listRefs(lines);
    if (v2 && !lines.includes('command=fetch')) throw new Error('unknown git command');
    const args = (name: string) => lines.filter((line) => line.startsWith(name + ' ')).map((line) => line.slice(name.length + 1));
    const wants = args('want').map((line) => line.split(' ')[0]);
    const haves = args('have').filter((oid) => this.store.objects.has(oid));
    const oldShallow = new Set(args('shallow'));
    if (wants.some((oid) => !this.store.objects.has(oid))) {
      return Buffer.concat([packet('ERR unknown object requested\n'), flush]);
    }
    const depthArg = args('deepen')[0];
    const depth = depthArg === undefined ? Infinity : Number(depthArg);
    if (depth !== Infinity && (!Number.isSafeInteger(depth) || depth < 1)) throw new Error('invalid fetch depth');
    if (!wants.length) return flush;
    const done = lines.includes('done');
    const acknowledgments = v2 && !done
      ? packets(['acknowledgments\n', ...(haves.length ? haves.map((oid) => 'ACK ' + oid + '\n') : ['NAK\n'])]) : [];
    if (v2 && !done && (!haves.length || lines.includes('wait-for-done'))) {
      return Buffer.concat([...acknowledgments, flush]);
    }
    const relative = depth !== Infinity && lines.includes('deepen-relative') && oldShallow.size > 0;
    const cacheKey = !haves.length && !oldShallow.size && v2 && depth === 1 && done ? wants.join(' ') : null;
    if (cacheKey && this.cachedFetch?.key === cacheKey) return this.cachedFetch.body;
    const { selected, shallow } = this.reach(wants, depth, relative || depth === Infinity ? oldShallow : new Set(), relative);
    const held = haves.length ? this.reach(haves, Infinity, oldShallow).selected : new Set<string>();
    const boundaries = [
      ...[...shallow].filter((oid) => !oldShallow.has(oid)).map((oid) => 'shallow ' + oid + '\n'),
      ...[...oldShallow].filter((oid) => selected.has(oid) && !shallow.has(oid)).map((oid) => 'unshallow ' + oid + '\n'),
    ];
    const pack = packObjects([...selected].filter((oid) => !held.has(oid)).map((oid) => this.store.objects.get(oid)!));
    const wantsLine = lines.find((line) => line.startsWith('want ')) ?? '';
    let result: Buffer;
    if (v2) {
      const sections = boundaries.length ? [packet('shallow-info\n'), ...packets(boundaries), delimiter] : [];
      const negotiation = acknowledgments.length ? [...acknowledgments, packet('ready\n'), delimiter] : [];
      result = Buffer.concat([...negotiation, ...sections, packet('packfile\n'), ...sideband(pack), flush]);
    } else {
      const prefix = [...packets(boundaries), ...(depth !== Infinity ? [flush] : []), packet('NAK\n')];
      const useBand = /\bside-band(?:-64k)?\b/.test(wantsLine);
      result = Buffer.concat(useBand ? [...prefix, ...sideband(pack), flush] : [...prefix, pack]);
    }
    if (cacheKey && result.length <= 4 * 1024 * 1024) this.cachedFetch = { key: cacheKey, body: result };
    return result;
  }

  checkGraph(root: string, extra: Map<string, GitObject>): Set<string> | null {
    const pending: Array<[string, string?]> = [[root]];
    const seen = new Set<string>();
    while (pending.length) {
      const [oid, kind] = pending.pop()!;
      const stored = this.store.objects.get(oid);
      const object = stored ?? extra.get(oid);
      if (!object || (kind && kind !== object.kind)) return null;
      if (stored || seen.has(oid)) continue;
      seen.add(oid);
      if (object.kind === 'commit') {
        const commit = readCommit(object.bytes);
        pending.push([commit.tree, 'tree']);
        for (const parent of commit.parents) pending.push([parent, 'commit']);
      } else if (object.kind === 'tree') {
        for (const entry of readTree(object.bytes)) {
          if (entry.mode !== '160000') pending.push([entry.oid, Number(entry.mode) === 40000 ? 'tree' : 'blob']);
        }
      } else if (object.kind === 'tag') {
        pending.push([tagTarget(object)]);
      }
    }
    return seen;
  }

  receive(body: Buffer): Buffer {
    const { lines, offset } = readPackets(body, true);
    if (!lines.length) return flush;
    const capabilities = lines[0].split('\0')[1]?.split(' ') ?? [];
    const commands = lines.map((line) => {
      const [oldOid, newOid, ref] = line.split('\0')[0].split(' ');
      if (!isOid(oldOid ?? '') || !isOid(newOid ?? '') || !validRef(ref ?? '')) throw new Error('invalid push command');
      return { oldOid, newOid, ref };
    });
    const report: string[] = [];
    let extra = new Map<string, GitObject>();
    let unpackError = false;
    if (offset < body.length || commands.some((cmd) => cmd.newOid !== ZERO)) {
      try {
        extra = unpack(body.subarray(offset), this.store);
        let newBytes = 0;
        for (const [oid, object] of extra) if (!this.store.objects.has(oid)) newBytes += object.bytes.length + (object.compressed?.length ?? 0);
        if (this.store.bytes + newBytes > 192 * 1024 * 1024) throw new Error('object store is full');
      } catch {
        unpackError = true;
      }
    }
    report.push(unpackError ? 'unpack invalid pack\n' : 'unpack ok\n');
    const accepted = new Set<string>();
    for (const { oldOid, newOid, ref } of commands) {
      let reason = '';
      let reachable: Set<string> | null = null;
      const current = this.refs.get(ref) ?? ZERO;
      const branch = ref.startsWith('refs/heads/');
      if (unpackError) reason = 'unpacker error';
      else if (current !== oldOid) reason = 'stale info';
      else if (newOid !== ZERO) {
        try {
          const tip = extra.get(newOid) ?? this.store.objects.get(newOid);
          reachable = this.checkGraph(newOid, extra);
          if ((branch && tip?.kind !== 'commit') || !reachable) reason = 'missing or invalid object';
          else if (oldOid !== ZERO && newOid !== oldOid && (!branch || !this.isAncestor(oldOid, newOid, extra))) reason = 'non-fast-forward';
        } catch {
          reason = 'invalid object';
        }
      }
      if (reason) {
        report.push('ng ' + ref + ' ' + reason + '\n');
      } else {
        if (newOid === ZERO) this.refs.delete(ref);
        else this.refs.set(ref, newOid);
        if (reachable) for (const oid of reachable) accepted.add(oid);
        report.push('ok ' + ref + '\n');
      }
    }
    for (const oid of accepted) this.store.set(oid, extra.get(oid)!);
    if (!capabilities.includes('report-status') && !capabilities.includes('report-status-v2')) return flush;
    const stream = Buffer.concat([...packets(report), flush]);
    return capabilities.includes('side-band-64k') ? Buffer.concat([...sideband(stream), flush]) : stream;
  }
}
