/* Reading a session trace, in the browser and nowhere else.
 *
 * The backend serves the sealed file exactly as it sits on disk. The private
 * key that opens it is typed into this page, kept in memory for as long as the
 * tab is open, and never sent anywhere — there is no endpoint that would take
 * it. Everything below runs client-side for that one reason.
 *
 * File format (trace/readme.md is the authority):
 *   line 1   {"v":1,"session":…,"keys":[{"for":"org"|<user id>,"wrapped":…}]}
 *   line n   {"seq":n,"c":"<base64 nonce ‖ ciphertext>"}
 *   line n   {"seq":n,"grant":<user id>,"wrapped":…}
 */
import { Decrypter, Encrypter } from "age-encryption";
import { chacha20poly1305 } from "@noble/ciphers/chacha.js";

/** One decrypted record. Payload is free-form per kind. */
export interface TraceRecord {
  at: string;
  session: string;
  actor: string;
  kind: string;
  payload?: unknown;
  /** The line's position in the file, so a gap is visible as one. */
  seq: number;
}

export class TraceKeyError extends Error {}

function fromBase64(s: string): Uint8Array {
  const raw = atob(s);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

function toBase64(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

/** A parsed line, before anything is decrypted. */
interface Line {
  seq?: number;
  v?: number;
  c?: string;
  grant?: string;
  wrapped?: string;
  keys?: Array<{ for: string; wrapped: string }>;
}

function parseLines(file: string): Line[] {
  return file
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => JSON.parse(l) as Line);
}

/**
 * The session key, if this identity can reach it. Key material is gathered
 * from the whole file before anything is decrypted, because a grant is
 * appended when it is issued and so sits after the records it opens.
 */
async function sessionKey(lines: Line[], identity: string): Promise<Uint8Array | null> {
  const wrapped: string[] = [];
  for (const k of lines[0]?.keys ?? []) wrapped.push(k.wrapped);
  for (const line of lines) if (line.grant && line.wrapped) wrapped.push(line.wrapped);

  for (const candidate of wrapped) {
    const d = new Decrypter();
    d.addIdentity(identity);
    try {
      const key = await d.decrypt(fromBase64(candidate));
      if (key.length === 32) return key;
    } catch {
      // Not a copy this identity opens. Try the next.
    }
  }
  return null;
}

/**
 * Decrypt a whole trace file. Throws TraceKeyError when the key does not open
 * this session, which is a different thing from the file being unreadable and
 * is worth saying differently.
 */
export async function decryptTrace(file: string, identity: string): Promise<TraceRecord[]> {
  const lines = parseLines(file);
  if (lines.length === 0) return [];
  if (!lines[0]?.v) throw new Error("That file is not a trace: it has no header.");

  const key = await sessionKey(lines, identity);
  if (!key) throw new TraceKeyError("This key does not open that session.");

  const out: TraceRecord[] = [];
  for (const line of lines) {
    if (!line.c) continue;
    const raw = fromBase64(line.c);
    const nonce = raw.subarray(0, 12);
    const plaintext = chacha20poly1305(key, nonce).decrypt(raw.subarray(12));
    const rec = JSON.parse(new TextDecoder().decode(plaintext)) as TraceRecord;
    out.push({ ...rec, seq: line.seq ?? 0 });
  }
  return out;
}

/**
 * Re-wrap a session's key for another recipient: what an administrator does
 * to give a developer back access to sessions their old key opened. The
 * plaintext key exists here for as long as this function runs and is never
 * sent — only the re-wrapped copy goes to the backend, which cannot open it.
 */
export async function regrant(file: string, identity: string, recipient: string): Promise<string> {
  const key = await sessionKey(parseLines(file), identity);
  if (!key) throw new TraceKeyError("This key does not open that session.");
  const e = new Encrypter();
  e.addRecipient(recipient);
  return toBase64(await e.encrypt(key));
}

/** A rough check before we bother the network: does this look like a key? */
export function looksLikeIdentity(key: string): boolean {
  return /^AGE-SECRET-KEY-1[0-9A-Z]+$/.test(key.trim().toUpperCase());
}
