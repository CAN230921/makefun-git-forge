import { createHash, hash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO = '0'.repeat(40);
export const isOid = (value: string) => /^[0-9a-f]{40}$/.test(value);
const MAX_OBJECT = 32 * 1024 * 1024;
const MAX_EXPANSION = 128 * 1024 * 1024;

export type Kind = 'commit' | 'tree' | 'blob' | 'tag';
export type GitObject = { kind: Kind; bytes: Buffer; compressed?: Buffer };
export type TreeEntry = { name: string; mode: string; oid: string };
const kinds = [null, 'commit', 'tree', 'blob', 'tag'] as const;
const digest = (bytes: Buffer) => hash('sha1', bytes, 'buffer');

export function objectId(kind: Kind, bytes: Buffer): string {
  const header = kind + ' ' + bytes.length + '\0';
  // Small objects benefit from a single hash call. Stream larger ones to avoid copying their bytes.
  if (bytes.length <= 1536) return hash('sha1', Buffer.concat([Buffer.from(header), bytes]), 'hex');
  return createHash('sha1').update(header).update(bytes).digest('hex');
}

export function readCommit(bytes: Buffer) {
  const text = bytes.toString('utf8');
  const boundary = text.indexOf('\n\n');
  if (boundary < 0) throw new Error('invalid commit');
  const headers = text.slice(0, boundary).split('\n');
  const tree = headers[0]?.startsWith('tree ') ? headers[0].slice(5) : '';
  const parents: string[] = [];
  for (let index = 1; headers[index]?.startsWith('parent '); index++) parents.push(headers[index].slice(7));
  if (!isOid(tree) || !parents.every(isOid)) throw new Error('invalid commit links');
  return { tree, parents, message: text.slice(boundary + 2).replace(/\n$/, '') };
}

export function readTree(bytes: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    const space = bytes.indexOf(32, offset);
    const nul = bytes.indexOf(0, offset);
    if (space < offset || nul <= space + 1 || nul + 21 > bytes.length) throw new Error('invalid tree');
    const mode = bytes.toString('ascii', offset, space);
    const name = bytes.toString('utf8', space + 1, nul);
    if (!/^(40000|040000|100644|100755|120000|160000)$/.test(mode) || name.includes('/') || name === '.' || name === '..') throw new Error('invalid tree entry');
    entries.push({ mode, name, oid: bytes.toString('hex', nul + 1, nul + 21) });
    offset = nul + 21;
  }
  return entries;
}

export class ObjectStore {
  objects = new Map<string, GitObject>();
  commits = new Map<string, ReturnType<typeof readCommit>>();
  trees = new Map<string, TreeEntry[]>();
  bytes = 0;

  put(kind: Kind, bytes: Buffer): string {
    const oid = objectId(kind, bytes);
    this.set(oid, { kind, bytes });
    return oid;
  }

  set(oid: string, object: GitObject) {
    if (this.objects.has(oid)) return;
    this.objects.set(oid, object);
    this.bytes += object.bytes.length + (object.compressed?.length ?? 0);
  }

  commit(oid: string) {
    const known = this.commits.get(oid);
    if (known) return known;
    const object = this.objects.get(oid);
    if (object?.kind !== 'commit') return undefined;
    const value = readCommit(object.bytes);
    this.commits.set(oid, value);
    return value;
  }

  tree(oid: string) {
    const known = this.trees.get(oid);
    if (known) return known;
    const object = this.objects.get(oid);
    if (object?.kind !== 'tree') return undefined;
    const value = readTree(object.bytes);
    this.trees.set(oid, value);
    return value;
  }
}

function readDeltaSize(delta: Buffer, cursor: { offset: number }): number {
  let value = 0;
  for (let shift = 0; shift <= 28 && cursor.offset < delta.length; shift += 7) {
    const byte = delta[cursor.offset++];
    value += (byte & 127) * 2 ** shift;
    if (!(byte & 128) && value <= MAX_OBJECT) return value;
  }
  throw new Error('invalid delta length');
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  const cursor = { offset: 0 };
  if (readDeltaSize(delta, cursor) !== base.length) throw new Error('delta base size mismatch');
  const result = Buffer.allocUnsafe(readDeltaSize(delta, cursor));
  let written = 0;
  const readByte = () => {
    if (cursor.offset >= delta.length) throw new Error('truncated delta');
    return delta[cursor.offset++];
  };
  while (cursor.offset < delta.length) {
    const command = readByte();
    if (command & 128) {
      let from = 0;
      let size = 0;
      for (let i = 0; i < 4; i++) if (command & (1 << i)) from += readByte() * 2 ** (8 * i);
      for (let i = 0; i < 3; i++) if (command & (16 << i)) size += readByte() * 2 ** (8 * i);
      size ||= 65536;
      if (from + size > base.length || written + size > result.length) throw new Error('invalid delta copy');
      written += base.copy(result, written, from, from + size);
    } else {
      if (!command || cursor.offset + command > delta.length || written + command > result.length) throw new Error('invalid delta insert');
      written += delta.copy(result, written, cursor.offset, cursor.offset + command);
      cursor.offset += command;
    }
  }
  if (written !== result.length) throw new Error('incomplete delta');
  return result;
}

export function unpack(pack: Buffer, existing: ObjectStore): Map<string, GitObject> {
  const end = pack.length - 20;
  if (end < 12 || pack.toString('ascii', 0, 4) !== 'PACK' || ![2, 3].includes(pack.readUInt32BE(4))) throw new Error('invalid pack header');
  if (!digest(pack.subarray(0, end)).equals(pack.subarray(end))) throw new Error('pack checksum mismatch');
  const count = pack.readUInt32BE(8);
  if (count > 50000) throw new Error('too many packed objects');
  const newObjects = new Map<string, GitObject>();
  const offsets = new Map<number, GitObject>();
  const deltas: Array<{ at: number; offsetBase?: number; refBase?: string; bytes: Buffer }> = [];
  let expanded = 0;
  let cursor = 12;
  const readByte = () => {
    if (cursor >= end) throw new Error('truncated pack');
    return pack[cursor++];
  };
  const add = (at: number, object: GitObject) => {
    newObjects.set(objectId(object.kind, object.bytes), object);
    offsets.set(at, object);
  };

  for (let i = 0; i < count; i++) {
    const at = cursor;
    let byte = readByte();
    const type = (byte >> 4) & 7;
    let size = byte & 15;
    for (let shift = 4; byte & 128; shift += 7) {
      if (shift > 25) throw new Error('invalid pack object size');
      byte = readByte();
      size += (byte & 127) * 2 ** shift;
    }
    expanded += size;
    if (size > MAX_OBJECT || expanded > MAX_EXPANSION) throw new Error('pack exceeds its inflated size budget');
    let offsetBase: number | undefined;
    let refBase: string | undefined;
    if (type === 6) {
      byte = readByte();
      let distance = byte & 127;
      while (byte & 128) {
        byte = readByte();
        distance = (distance + 1) * 128 + (byte & 127);
        if (distance > at - 12) throw new Error('invalid delta offset');
      }
      if (!distance || distance > at - 12) throw new Error('invalid delta offset');
      offsetBase = at - distance;
    } else if (type === 7) {
      if (cursor + 20 > end) throw new Error('missing delta object id');
      refBase = pack.toString('hex', cursor, cursor + 20);
      cursor += 20;
    } else if (!kinds[type]) {
      throw new Error('invalid pack object type');
    }
    const inflated = inflateSync(pack.subarray(cursor, end), { info: true, maxOutputLength: size + 1 }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    if (inflated.buffer.length !== size || !inflated.engine.bytesWritten) throw new Error('pack object size mismatch');
    const next = cursor + inflated.engine.bytesWritten;
    if (offsetBase !== undefined || refBase !== undefined) {
      deltas.push({ at, offsetBase, refBase, bytes: inflated.buffer });
    } else {
      add(at, { kind: kinds[type]!, bytes: inflated.buffer, compressed: pack.subarray(cursor, next) });
    }
    cursor = next;
  }
  if (cursor !== end) throw new Error('unexpected trailing pack bytes');
  let pending = deltas;
  while (pending.length) {
    const later: typeof deltas = [];
    for (const entry of pending) {
      const base = entry.refBase === undefined
        ? offsets.get(entry.offsetBase!)
        : newObjects.get(entry.refBase) ?? existing.objects.get(entry.refBase);
      if (!base) {
        later.push(entry);
        continue;
      }
      const bytes = applyDelta(base.bytes, entry.bytes);
      expanded += bytes.length;
      if (expanded > MAX_EXPANSION) throw new Error('pack delta expansion exceeds its budget');
      add(entry.at, { kind: base.kind, bytes });
    }
    if (later.length === pending.length) throw new Error('missing delta base');
    pending = later;
  }
  return newObjects;
}

function packObjectHeader(kind: Kind, length: number): Buffer {
  const header = [(kinds.indexOf(kind) << 4) | (length & 15)];
  let rest = Math.floor(length / 16);
  while (rest) {
    header[header.length - 1] |= 128;
    header.push(rest & 127);
    rest = Math.floor(rest / 128);
  }
  return Buffer.from(header);
}

export function packObjects(objects: Iterable<GitObject>): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK');
  header.writeUInt32BE(2, 4);
  const chunks = [header];
  let count = 0;
  for (const object of objects) {
    count++;
    object.compressed ??= deflateSync(object.bytes, { level: 1 });
    chunks.push(packObjectHeader(object.kind, object.bytes.length), object.compressed);
  }
  header.writeUInt32BE(count, 8);
  const hash = createHash('sha1');
  for (const chunk of chunks) hash.update(chunk);
  chunks.push(hash.digest());
  return Buffer.concat(chunks);
}
