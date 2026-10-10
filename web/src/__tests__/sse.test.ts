import { afterEach, describe, expect, it, vi } from "vitest";
import { auth, parseSSE, subscribeJobEvents } from "../lib/api";

describe("parseSSE", () => {
  it("parses complete frames and keeps the remainder", () => {
    const { frames, rest } = parseSSE('id: 1\nevent: job.status_changed\ndata: {"a":1}\n\nid: 2\nevent: job.lo');
    expect(frames).toEqual([{ id: "1", event: "job.status_changed", data: '{"a":1}' }]);
    expect(rest).toBe("id: 2\nevent: job.lo");
  });

  it("follows the spec: CRLF, no space after colon, multi-line data, comments", () => {
    const { frames } = parseSSE(": keep-alive\r\n\r\nid:7\r\nevent:x\r\ndata:line1\r\ndata: line2\r\n\r\n");
    expect(frames).toEqual([{ id: "7", event: "x", data: "line1\nline2" }]);
  });

  it("reassembles frames split across arbitrary chunk boundaries", () => {
    const wire = 'id: 1\nevent: a\ndata: {"n":1}\n\nid: 2\nevent: b\ndata: {"n":2}\n\n';
    for (let cut = 1; cut < wire.length; cut++) {
      const first = parseSSE(wire.slice(0, cut));
      const second = parseSSE(first.rest + wire.slice(cut));
      expect([...first.frames, ...second.frames].map((f) => f.id)).toEqual(["1", "2"]);
    }
  });
});

function sseResponse(body: string, status = 200): Response {
  const bytes = new TextEncoder().encode(body);
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(bytes);
      c.close();
    },
  });
  return new Response(stream, { status, headers: { "Content-Type": "text/event-stream" } });
}

describe("subscribeJobEvents", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    sessionStorage.clear();
  });

  it("sends the token as a header (never in the URL), resumes with Last-Event-ID, and stops on end", async () => {
    auth.set("secret-token");
    const fetchMock = vi
      .fn()
      // First connection drops after event 5 without an `end`.
      .mockResolvedValueOnce(sseResponse('id: 5\nevent: job.status_changed\ndata: {"id":5}\n\n'))
      .mockResolvedValueOnce(sseResponse('id: 6\nevent: job.completed\ndata: {"id":6}\n\nid: 7\nevent: end\ndata: {}\n\n'));
    vi.stubGlobal("fetch", fetchMock);
    const events: unknown[] = [];
    const ended = new Promise<void>((resolve) => subscribeJobEvents("job_1", (e) => events.push(e), resolve));
    await ended;

    expect(events).toEqual([{ id: 5 }, { id: 6 }]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    for (const [url, init] of fetchMock.mock.calls) {
      expect(url).toBe("/api/v1/jobs/job_1/events");
      expect(String(url)).not.toContain("secret-token");
      expect(init.headers.Authorization).toBe("Bearer secret-token");
    }
    expect(fetchMock.mock.calls[0][1].headers["Last-Event-ID"]).toBeUndefined();
    expect(fetchMock.mock.calls[1][1].headers["Last-Event-ID"]).toBe("5");
  });

  it("gives up without retrying on 401", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("", { status: 401 }));
    vi.stubGlobal("fetch", fetchMock);
    const onEnd = vi.fn();
    subscribeJobEvents("job_1", () => {}, onEnd);
    await new Promise((r) => setTimeout(r, 50));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(onEnd).not.toHaveBeenCalled();
  });
});
