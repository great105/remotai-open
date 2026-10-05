import { expect, it, vi } from "vitest";
import { createHermesClient, type RequestTransport } from "./client";

it("bootstraps with a cursor-only host request and forwards cancellation", async () => {
 const result = { events: [], latest_seq: 42, reset: false };
 const request = vi.fn(async () => result);
 const client = createHermesClient(() => ({ request } as unknown as RequestTransport));
 const signal = new AbortController().signal;
 expect(await client.eventCursor(signal)).toBe(result);
 expect(request).toHaveBeenCalledWith("/api/hermes/events?cursor=1", { signal });
});

it("long-polls the authenticated host with a bounded wait and abort signal", async () => {
 const request = vi.fn(async () => ({ events: [], latest_seq: 42 }));
 const client = createHermesClient(() => ({ request } as unknown as RequestTransport));
 const signal = new AbortController().signal;
 await client.waitEvents(42, signal);
 expect(request).toHaveBeenLastCalledWith("/api/hermes/events?after=42&wait_ms=20000", { signal });
 await client.waitEvents(42, signal, 50000);
 expect(request).toHaveBeenLastCalledWith("/api/hermes/events?after=42&wait_ms=20000", { signal });
 await client.waitEvents(42, signal, 125);
 expect(request).toHaveBeenLastCalledWith("/api/hermes/events?after=42&wait_ms=125", { signal });
 await client.events(42, signal);
 expect(request).toHaveBeenLastCalledWith("/api/hermes/events?after=42", { signal });
});

it.each(["eventCursor", "waitEvents"] as const)("fences %s against device changes before and after transport", async method => {
 const request = vi.fn(async () => ({ events: [], latest_seq: 0 }));
 let stale = true;
 const client = createHermesClient(() => ({ request } as unknown as RequestTransport), () => { if (stale) throw new Error("device changed"); });
 const load = () => method === "eventCursor" ? client.eventCursor() : client.waitEvents(0);
 await expect(load()).rejects.toThrow("device changed");
 expect(request).not.toHaveBeenCalled();
 stale = false;
 request.mockImplementationOnce(async () => { stale = true; return { events: [], latest_seq: 0 }; });
 await expect(load()).rejects.toThrow("device changed");
 expect(request).toHaveBeenCalledTimes(1);
});

it("in-flight event waiting preserves transport abort rejection", async () => {
 const controller = new AbortController();
 const request = vi.fn((_path: string, init?: RequestInit) => new Promise((_resolve, reject) => {
   init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true });
 }));
 const client = createHermesClient(() => ({ request } as unknown as RequestTransport));
 const promise = client.waitEvents(0, controller.signal);
 controller.abort();
 await expect(promise).rejects.toMatchObject({ name: "AbortError" });
});

it("rejects invalid event waits before touching transport", async () => {
 const request = vi.fn();
 const client = createHermesClient(() => ({ request } as unknown as RequestTransport));
 for (const value of [-1, NaN, Infinity, 1.5]) await expect(client.waitEvents(0, undefined, value)).rejects.toThrow();
 expect(request).not.toHaveBeenCalled();
});
