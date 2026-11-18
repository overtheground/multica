import { describe, expect, it } from "vitest";
import {
  DEFAULT_CONTEXT_THRESHOLD,
  averageContext,
  cacheShare,
  contextLevel,
  parseThreshold,
} from "./context-utils";

describe("context utils", () => {
  it("colours by threshold", () => {
    expect(contextLevel(100_000, 200_000)).toBe("ok");
    expect(contextLevel(150_000, 200_000)).toBe("warn");
    expect(contextLevel(200_000, 200_000)).toBe("over");
  });
  it("averages per call", () => {
    expect(averageContext({ sum_tokens: 300, calls: 3 })).toBe(100);
    expect(averageContext({ sum_tokens: 0, calls: 0 })).toBe(0);
  });
  it("computes the cache share", () => {
    expect(cacheShare(1000, 900)).toBe(0.9);
    expect(cacheShare(0, 0)).toBe(0);
  });
  it("falls back to the default threshold on junk", () => {
    expect(parseThreshold(null)).toBe(0 + DEFAULT_CONTEXT_THRESHOLD);
    expect(parseThreshold("abc")).toBe(DEFAULT_CONTEXT_THRESHOLD);
    expect(parseThreshold("300000")).toBe(300_000);
  });
});
