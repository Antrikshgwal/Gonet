// Minimal MessagePack codec for this game's wire format. Covers the subset the
// server emits/accepts: nil, bool, ints, float32/64, strings, arrays, maps.

export function encode(value) {
  const bytes = [];
  writeValue(bytes, value);
  return new Uint8Array(bytes);
}

function writeValue(b, v) {
  if (v === null || v === undefined) { b.push(0xc0); return; }
  if (typeof v === "boolean") { b.push(v ? 0xc3 : 0xc2); return; }
  if (typeof v === "number") { Number.isInteger(v) ? writeInt(b, v) : writeFloat64(b, v); return; }
  if (typeof v === "string") { writeStr(b, v); return; }
  if (Array.isArray(v)) { writeArray(b, v); return; }
  writeMap(b, v);
}

function writeInt(b, n) {
  if (n >= 0) {
    if (n < 0x80) { b.push(n); }
    else if (n < 0x100) { b.push(0xcc, n); }
    else if (n < 0x10000) { b.push(0xcd, n >> 8, n & 0xff); }
    else if (n < 0x100000000) { b.push(0xce, (n >>> 24) & 0xff, (n >> 16) & 0xff, (n >> 8) & 0xff, n & 0xff); }
    else { writeUint64(b, n); }
  } else {
    if (n >= -32) { b.push(n & 0xff); }
    else if (n >= -128) { b.push(0xd0, n & 0xff); }
    else if (n >= -32768) { b.push(0xd1, (n >> 8) & 0xff, n & 0xff); }
    else { b.push(0xd2, (n >>> 24) & 0xff, (n >> 16) & 0xff, (n >> 8) & 0xff, n & 0xff); }
  }
}

function writeUint64(b, n) {
  b.push(0xcf);
  const hi = Math.floor(n / 0x100000000), lo = n >>> 0;
  b.push((hi >> 24) & 0xff, (hi >> 16) & 0xff, (hi >> 8) & 0xff, hi & 0xff,
         (lo >> 24) & 0xff, (lo >> 16) & 0xff, (lo >> 8) & 0xff, lo & 0xff);
}

function writeFloat64(b, n) {
  const dv = new DataView(new ArrayBuffer(8));
  dv.setFloat64(0, n);
  b.push(0xcb);
  for (let i = 0; i < 8; i++) b.push(dv.getUint8(i));
}

function writeStr(b, s) {
  const utf8 = new TextEncoder().encode(s);
  const len = utf8.length;
  if (len < 0x20) b.push(0xa0 | len);
  else if (len < 0x100) b.push(0xd9, len);
  else b.push(0xda, len >> 8, len & 0xff);
  for (const byte of utf8) b.push(byte);
}

function writeArray(b, arr) {
  const len = arr.length;
  if (len < 0x10) b.push(0x90 | len);
  else b.push(0xdc, len >> 8, len & 0xff);
  for (const item of arr) writeValue(b, item);
}

function writeMap(b, obj) {
  const keys = Object.keys(obj);
  if (keys.length < 0x10) b.push(0x80 | keys.length);
  else b.push(0xde, keys.length >> 8, keys.length & 0xff);
  for (const k of keys) { writeStr(b, k); writeValue(b, obj[k]); }
}

export function decode(buf) {
  const dv = new DataView(buf.buffer || buf, buf.byteOffset || 0, buf.byteLength);
  const r = { dv, pos: 0 };
  return readValue(r);
}

function readValue(r) {
  const c = r.dv.getUint8(r.pos++);
  if (c < 0x80) return c;                       // positive fixint
  if (c < 0x90) return readMap(r, c & 0x0f);    // fixmap
  if (c < 0xa0) return readArray(r, c & 0x0f);  // fixarray
  if (c < 0xc0) return readStr(r, c & 0x1f);    // fixstr
  if (c >= 0xe0) return c - 0x100;              // negative fixint
  switch (c) {
    case 0xc0: return null;
    case 0xc2: return false;
    case 0xc3: return true;
    case 0xca: { const v = r.dv.getFloat32(r.pos); r.pos += 4; return v; }
    case 0xcb: { const v = r.dv.getFloat64(r.pos); r.pos += 8; return v; }
    case 0xcc: return r.dv.getUint8(r.pos++);
    case 0xcd: { const v = r.dv.getUint16(r.pos); r.pos += 2; return v; }
    case 0xce: { const v = r.dv.getUint32(r.pos); r.pos += 4; return v; }
    case 0xcf: { const hi = r.dv.getUint32(r.pos), lo = r.dv.getUint32(r.pos + 4); r.pos += 8; return hi * 0x100000000 + lo; }
    case 0xd0: return r.dv.getInt8(r.pos++);
    case 0xd1: { const v = r.dv.getInt16(r.pos); r.pos += 2; return v; }
    case 0xd2: { const v = r.dv.getInt32(r.pos); r.pos += 4; return v; }
    case 0xd3: { const hi = r.dv.getInt32(r.pos), lo = r.dv.getUint32(r.pos + 4); r.pos += 8; return hi * 0x100000000 + lo; }
    case 0xd9: return readStr(r, r.dv.getUint8(r.pos++));
    case 0xda: { const len = r.dv.getUint16(r.pos); r.pos += 2; return readStr(r, len); }
    case 0xdc: { const len = r.dv.getUint16(r.pos); r.pos += 2; return readArray(r, len); }
    case 0xde: { const len = r.dv.getUint16(r.pos); r.pos += 2; return readMap(r, len); }
  }
  throw new Error("unsupported msgpack byte 0x" + c.toString(16));
}

function readStr(r, len) {
  const bytes = new Uint8Array(r.dv.buffer, r.dv.byteOffset + r.pos, len);
  r.pos += len;
  return new TextDecoder().decode(bytes);
}

function readArray(r, len) {
  const out = new Array(len);
  for (let i = 0; i < len; i++) out[i] = readValue(r);
  return out;
}

function readMap(r, len) {
  const out = {};
  for (let i = 0; i < len; i++) { const k = readValue(r); out[k] = readValue(r); }
  return out;
}
