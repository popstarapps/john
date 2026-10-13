/* The console's half of the API contract.
 *
 * Every mutation here has a handler on the Go side that these calls have to
 * agree with — the path, the method, and the field names in the body. The Go
 * tests prove the backend answers those requests correctly; these prove the
 * console makes them, which is the half a typecheck cannot see.
 *
 * Nothing is mocked below the fetch call: the request that would go over the
 * wire is the thing being asserted.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  commentOnNetworkAccessRequest,
  deleteModel,
  deleteTeam,
  getVersion,
  setUserRole,
  setUserStatus,
  subscribe,
  updateModel,
} from "./client";

interface Call {
  method: string;
  path: string;
  body: unknown;
  contentType: string | undefined;
}

let calls: Call[] = [];

/** Answers every request with `payload`, recording what was asked for. */
function stubFetch(payload: unknown = { ok: true }, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const headers = new Headers(init?.headers as HeadersInit | undefined);
      calls.push({
        method: init?.method ?? "GET",
        path: input,
        body: init?.body === undefined ? undefined : JSON.parse(init.body as string),
        contentType: headers.get("Content-Type") ?? undefined,
      });
      return {
        ok: status >= 200 && status < 300,
        status,
        json: async () => payload,
        text: async () => JSON.stringify(payload),
      } as Response;
    }),
  );
}

const only = (): Call => {
  expect(calls).toHaveLength(1);
  return calls[0];
};

beforeEach(() => {
  calls = [];
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("people", () => {
  it("promotes and demotes through the role endpoint", async () => {
    stubFetch({ id: "u_1", role: "admin" });
    const user = await setUserRole("u_1", "admin");

    expect(only()).toMatchObject({
      method: "PUT",
      path: "/api/v1/users/u_1/role",
      body: { role: "admin" },
      contentType: "application/json",
    });
    // The updated person comes back, so the page re-renders from the answer
    // rather than from what it hoped it had done.
    expect(user).toMatchObject({ id: "u_1", role: "admin" });
  });

  it("suspends and reactivates through the status endpoint", async () => {
    stubFetch({ id: "u_1", status: "suspended" });
    await setUserStatus("u_1", "suspended");
    expect(only()).toMatchObject({
      method: "PUT",
      path: "/api/v1/users/u_1/status",
      body: { status: "suspended" },
    });

    calls = [];
    stubFetch({ id: "u_1", status: "active" });
    await setUserStatus("u_1", "active");
    expect(only().body).toEqual({ status: "active" });
  });

  it("escapes an id rather than pasting it into the path", async () => {
    stubFetch();
    await setUserRole("u_1/../admin", "admin");
    expect(only().path).toBe("/api/v1/users/u_1%2F..%2Fadmin/role");
  });
});

describe("teams", () => {
  it("deletes by id, with no body to send", async () => {
    stubFetch();
    await deleteTeam("t_1");
    const call = only();
    expect(call.method).toBe("DELETE");
    expect(call.path).toBe("/api/v1/teams/t_1");
    expect(call.body).toBeUndefined();
    // No body means no Content-Type: there is nothing to describe.
    expect(call.contentType).toBeUndefined();
  });
});

describe("models", () => {
  const draft = {
    name: "Claude Opus",
    provider: "anthropic",
    hosting: "external" as const,
    endpoint: "https://api.anthropic.com",
    modelId: "claude-opus-5",
    contextWindow: 1_000_000,
    costPerMTokIn: 5,
    costPerMTokCachedIn: 0.5,
    costPerMTokOut: 25,
    apiKey: "",
  };

  it("edits in place, and an empty key still goes so the backend keeps the stored one", async () => {
    stubFetch({ id: "m_1", name: "Claude Opus" });
    await updateModel("m_1", draft);

    const call = only();
    expect(call.method).toBe("PUT");
    expect(call.path).toBe("/api/v1/models/m_1");
    // The blank field is the contract: it means "leave the credential alone".
    // Dropping it would be indistinguishable from erasing it.
    expect(call.body).toMatchObject({ apiKey: "", modelId: "claude-opus-5", contextWindow: 1_000_000 });
    expect((call.body as { provider: string }).provider).toBe("anthropic");
  });

  it("sends a new key when one is typed", async () => {
    stubFetch({ id: "m_1" });
    await updateModel("m_1", { ...draft, apiKey: "sk-rotated" });
    expect((only().body as { apiKey: string }).apiKey).toBe("sk-rotated");
  });

  it("deletes by id", async () => {
    stubFetch();
    await deleteModel("m_1");
    expect(only()).toMatchObject({ method: "DELETE", path: "/api/v1/models/m_1" });
  });

  // The Models page shows this message in the toast, so it has to survive the
  // trip out of the client rather than becoming a generic failure.
  it("carries the backend's refusal out to the caller", async () => {
    stubFetch(
      { error: "still named by 1 model-routing policy: only the internal one." },
      409,
    );
    await expect(deleteModel("m_1")).rejects.toThrow(/only the internal one/);
  });
});

describe("access request conversation", () => {
  it("comments without moving the request", async () => {
    stubFetch({ id: "r_1", status: "pending", comments: [] });
    await commentOnNetworkAccessRequest("r_1", "The payments UI build.");
    expect(only()).toMatchObject({
      method: "POST",
      path: "/api/v1/access-requests/r_1/comments",
      body: { body: "The payments UI build.", needsInfo: false },
    });
  });

  it("hands the request back when a reviewer asks", async () => {
    stubFetch({ id: "r_1", status: "needs-info", comments: [] });
    const updated = await commentOnNetworkAccessRequest("r_1", "Which build?", {
      needsInfo: true,
    });
    expect((only().body as { needsInfo: boolean }).needsInfo).toBe(true);
    // The answer carries the new status, so the page does not have to guess.
    expect(updated.status).toBe("needs-info");
  });
});

// Every view reads through useQuery, which re-runs on this counter. A mutation
// that forgot to bump it would leave a stale page behind a successful change.
describe("subscriptions", () => {
  it("invalidates every live query after a mutation", async () => {
    stubFetch();
    let notified = 0;
    const stop = subscribe(() => {
      notified += 1;
    });
    const before = getVersion();

    await deleteTeam("t_1");
    await setUserRole("u_1", "member");

    expect(getVersion()).toBe(before + 2);
    expect(notified).toBe(2);
    stop();
  });
});
