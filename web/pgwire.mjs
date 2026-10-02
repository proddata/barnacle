// A tiny PostgreSQL simple-query client for the manual WebSocket check.
// The proxy itself does not parse PostgreSQL messages.
const encoder = new TextEncoder();
const decoder = new TextDecoder();

function join(...parts) {
  const result = new Uint8Array(parts.reduce((size, part) => size + part.length, 0));
  let offset = 0;
  for (const part of parts) { result.set(part, offset); offset += part.length; }
  return result;
}
function int32(value) {
  const bytes = new Uint8Array(4);
  new DataView(bytes.buffer).setInt32(0, value);
  return bytes;
}
function cstring(value) { return join(encoder.encode(value), new Uint8Array(1)); }
function packet(type, body) { return join(encoder.encode(type), int32(body.length + 4), body); }
function startup(user, database) {
  const body = join(int32(196608), cstring('user'), cstring(user), cstring('database'), cstring(database), new Uint8Array(1));
  return join(int32(body.length + 4), body);
}
function base64(bytes) {
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}
function unbase64(value) {
  const binary = atob(value);
  return Uint8Array.from(binary, char => char.charCodeAt(0));
}
async function hmac(key, text) {
  const imported = await crypto.subtle.importKey('raw', key, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  return new Uint8Array(await crypto.subtle.sign('HMAC', imported, encoder.encode(text)));
}
async function sha256(bytes) { return new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)); }
function attributes(text) {
  const result = Object.create(null);
  for (const item of text.split(',')) {
    const split = item.indexOf('=');
    if (split > 0) result[item.slice(0, split)] = item.slice(split + 1);
  }
  return result;
}
function readString(bytes, offset) {
  const end = bytes.indexOf(0, offset);
  if (end < 0) throw new Error('Invalid PostgreSQL string');
  return [decoder.decode(bytes.subarray(offset, end)), end + 1];
}
function readError(bytes) {
  const fields = Object.create(null);
  for (let offset = 0; offset < bytes.length && bytes[offset] !== 0;) {
    const key = String.fromCharCode(bytes[offset++]);
    const [value, next] = readString(bytes, offset);
    fields[key] = value;
    offset = next;
  }
  return new Error(fields.M || 'PostgreSQL error' + (fields.C ? ` (${fields.C})` : ''));
}
function readFields(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const count = view.getUint16(0);
  const fields = [];
  let offset = 2;
  for (let i = 0; i < count; i++) {
    const [name, next] = readString(bytes, offset);
    offset = next;
    const dataTypeID = view.getUint32(offset + 6);
    offset += 18;
    fields.push({ name, dataTypeID });
  }
  return fields;
}
function readRow(bytes, fields) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const count = view.getUint16(0);
  if (count !== fields.length) throw new Error('Invalid PostgreSQL row');
  const row = {};
  let offset = 2;
  for (let i = 0; i < count; i++) {
    const length = view.getInt32(offset); offset += 4;
    row[fields[i].name] = length < 0 ? null : decoder.decode(bytes.subarray(offset, offset + length));
    if (length >= 0) offset += length;
  }
  return row;
}

export function runWebSocketQuery(connectionString, sql, endpoint = `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/v2`) {
  const connection = new URL(connectionString);
  if (!['postgres:', 'postgresql:'].includes(connection.protocol)) throw new Error('Enter a PostgreSQL connection string');
  const user = decodeURIComponent(connection.username);
  const password = decodeURIComponent(connection.password);
  const database = decodeURIComponent(connection.pathname.slice(1) || user);
  if (!user || !password || !database) throw new Error('WebSocket queries need a connection string with username, password, and database');

  return new Promise((resolve, reject) => {
    const socket = new WebSocket(endpoint);
    socket.binaryType = 'arraybuffer';
    let settled = false;
    let buffer = new Uint8Array(0);
    let queue = Promise.resolve();
    let ready = false;
    let scram = null;
    let fields = [];
    let rows = [];
    const results = [];
    const timeout = setTimeout(() => fail(new Error('WebSocket query timed out')), 30000);
    function fail(error) {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      socket.close();
      reject(error);
    }
    function finish() {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      socket.close();
      resolve(results);
    }
    socket.onopen = () => socket.send(startup(user, database));
    socket.onerror = () => fail(new Error('WebSocket connection failed'));
    socket.onclose = () => { if (!settled) fail(new Error('WebSocket closed before the query finished')); };
    socket.onmessage = event => {
      queue = queue.then(async () => {
        const incoming = new Uint8Array(event.data instanceof Blob ? await event.data.arrayBuffer() : event.data);
        buffer = join(buffer, incoming);
        while (buffer.length >= 5) {
          const length = new DataView(buffer.buffer, buffer.byteOffset).getInt32(1);
          if (length < 4 || length > 64 * 1024 * 1024) throw new Error('Invalid PostgreSQL message length');
          if (buffer.length < length + 1) break;
          const type = String.fromCharCode(buffer[0]);
          const body = buffer.slice(5, length + 1);
          buffer = buffer.slice(length + 1);
          await handle(type, body);
        }
      }).catch(fail);
    };
    async function handle(type, body) {
      if (type === 'E') throw readError(body);
      if (type === 'R') {
        const code = new DataView(body.buffer, body.byteOffset).getInt32(0);
        if (code === 0) return;
        if (code === 3) { socket.send(packet('p', cstring(password))); return; }
        if (code === 10) {
          if (!decoder.decode(body.subarray(4)).split('\0').includes('SCRAM-SHA-256')) throw new Error('PostgreSQL did not offer SCRAM-SHA-256');
          const nonce = base64(crypto.getRandomValues(new Uint8Array(18)));
          const first = `n=${user.replaceAll('=', '=3D').replaceAll(',', '=2C')},r=${nonce}`;
          scram = { nonce, first };
          const clientFirst = encoder.encode(`n,,${first}`);
          socket.send(packet('p', join(cstring('SCRAM-SHA-256'), int32(clientFirst.length), clientFirst)));
          return;
        }
        if (code === 11 && scram) {
          const serverFirst = decoder.decode(body.subarray(4));
          const values = attributes(serverFirst);
          if (!values.r?.startsWith(scram.nonce) || values.r.length <= scram.nonce.length) throw new Error('Invalid SCRAM nonce');
          const iterations = Number(values.i);
          if (!Number.isInteger(iterations) || iterations < 1 || iterations > 1000000) throw new Error('Invalid SCRAM iteration count');
          const key = await crypto.subtle.importKey('raw', encoder.encode(password), 'PBKDF2', false, ['deriveBits']);
          const salted = new Uint8Array(await crypto.subtle.deriveBits({ name: 'PBKDF2', hash: 'SHA-256', salt: unbase64(values.s), iterations }, key, 256));
          const finalWithoutProof = `c=biws,r=${values.r}`;
          const authMessage = `${scram.first},${serverFirst},${finalWithoutProof}`;
          const clientKey = await hmac(salted, 'Client Key');
          const signature = await hmac(await sha256(clientKey), authMessage);
          const proof = clientKey.map((byte, index) => byte ^ signature[index]);
          scram.serverSignature = base64(await hmac(await hmac(salted, 'Server Key'), authMessage));
          socket.send(packet('p', encoder.encode(`${finalWithoutProof},p=${base64(proof)}`)));
          return;
        }
        if (code === 12 && scram) {
          const values = attributes(decoder.decode(body.subarray(4)));
          if (values.v !== scram.serverSignature) throw new Error('SCRAM server signature mismatch');
          scram.verified = true;
          return;
        }
        throw new Error(`Unsupported PostgreSQL authentication method ${code}`);
      }
      if (type === 'T') { fields = readFields(body); rows = []; return; }
      if (type === 'D') { rows.push(readRow(body, fields)); return; }
      if (type === 'C') {
        const [command] = readString(body, 0);
        results.push({ command, rows });
        fields = []; rows = [];
        return;
      }
      if (type === 'Z') {
        if (!ready) {
          if (scram && !scram.verified) throw new Error('SCRAM authentication was not verified');
          ready = true;
          socket.send(packet('Q', cstring(sql)));
        } else finish();
      }
    }
  });
}
