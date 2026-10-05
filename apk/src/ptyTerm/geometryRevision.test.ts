import { describe, expect, it } from "vitest";
import { noteTerminalGeometry, screenGeometryRevisionMatches } from "./geometryRevision";

describe("screen request geometry revision", () => {
  it("rejects an in-flight frame after the local xterm grid changes", () => {
    const initial = { revision: 0, geometry: { cols: 80, rows: 24 } } as const;
    const requestRevision = initial.revision;
    const resized = noteTerminalGeometry(initial, { cols: 48, rows: 30 });

    expect(resized.revision).toBe(1);
    expect(screenGeometryRevisionMatches(requestRevision, resized.revision)).toBe(false);
    expect(screenGeometryRevisionMatches(resized.revision, resized.revision)).toBe(true);
  });

  it("does not invalidate a frame for duplicate resize notifications", () => {
    const current = { revision: 7, geometry: { cols: 48, rows: 30 } } as const;
    expect(noteTerminalGeometry(current, { cols: 48, rows: 30 })).toBe(current);
  });

  it("rejects legacy or malformed echoes instead of adopting stale geometry", () => {
    for (const value of [undefined, null, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "4"]) {
      expect(screenGeometryRevisionMatches(value, 4)).toBe(false);
    }
  });
});
