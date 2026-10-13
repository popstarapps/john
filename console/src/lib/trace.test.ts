/* The console half of the trace format.
 *
 * This reads the very bytes the Go writer produced — the same fixture
 * trace/fixture_test.go opens — so the two implementations cannot drift apart
 * without one of them failing. Regenerate the fixture and fix both if the
 * format changes on purpose.
 */
import { identityToRecipient } from "age-encryption";
import { describe, expect, it } from "vitest";

import { decryptTrace, regrant, TraceKeyError, looksLikeIdentity } from "./trace";
import fixture from "./__fixtures__/trace-fixture.json";

describe("decryptTrace", () => {
  it("opens a session for the organization", async () => {
    const records = await decryptTrace(fixture.file, fixture.orgIdentity);
    expect(records.map((r) => r.kind)).toEqual(fixture.expectedKinds);
    expect(JSON.stringify(records[1].payload)).toContain(fixture.expectedPrompt);
  });

  it("opens the same session for the developer it belongs to", async () => {
    const records = await decryptTrace(fixture.file, fixture.userIdentity);
    expect(records.map((r) => r.kind)).toEqual(fixture.expectedKinds);
  });

  it("numbers records by their line, so a removed line leaves a gap", async () => {
    const records = await decryptTrace(fixture.file, fixture.orgIdentity);
    // The header is line 1, so the records run from 2.
    expect(records.map((r) => r.seq)).toEqual([2, 3, 4, 5]);
  });

  it("refuses a key that opens nothing in the file", async () => {
    await expect(decryptTrace(fixture.file, fixture.strangerIdentity)).rejects.toBeInstanceOf(
      TraceKeyError,
    );
  });
});

describe("regrant", () => {
  it("re-wraps a session key for another recipient without the backend", async () => {
    // A developer who has re-keyed: their new identity opens nothing yet.
    await expect(decryptTrace(fixture.file, fixture.strangerIdentity)).rejects.toBeInstanceOf(
      TraceKeyError,
    );

    const replacement = await identityToRecipient(fixture.strangerIdentity);
    const wrapped = await regrant(fixture.file, fixture.orgIdentity, replacement);
    const granted = `${fixture.file.trimEnd()}\n${JSON.stringify({
      seq: 6,
      grant: "u_dev",
      wrapped,
    })}\n`;

    const records = await decryptTrace(granted, fixture.strangerIdentity);
    expect(records.map((r) => r.kind)).toEqual(fixture.expectedKinds);
  });

  it("will not re-wrap with a key that cannot open the session", async () => {
    await expect(
      regrant(fixture.file, fixture.strangerIdentity, fixture.userRecipient),
    ).rejects.toBeInstanceOf(TraceKeyError);
  });
});

describe("looksLikeIdentity", () => {
  it("accepts a real identity and rejects the recipient half", () => {
    expect(looksLikeIdentity(fixture.orgIdentity)).toBe(true);
    expect(looksLikeIdentity(fixture.userRecipient)).toBe(false);
    expect(looksLikeIdentity("")).toBe(false);
    expect(looksLikeIdentity("hunter2")).toBe(false);
  });
});
