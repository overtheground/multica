import { describe, expect, it } from "vitest";
import {
  MAX_RANGE_DAYS,
  dimsForDays,
  parseCustomDays,
  rangeLabel,
} from "./dashboard-shared";

describe("parseCustomDays", () => {
  it("accepts a whole number of days", () => {
    expect(parseCustomDays("2")).toBe(2);
    expect(parseCustomDays(" 3 ")).toBe(3);
    expect(parseCustomDays("45")).toBe(45);
  });

  it("rounds fractions down and clamps to 1..MAX_RANGE_DAYS", () => {
    expect(parseCustomDays("2.9")).toBe(2);
    expect(parseCustomDays("0")).toBe(1);
    expect(parseCustomDays("-5")).toBe(1);
    expect(parseCustomDays("100000")).toBe(MAX_RANGE_DAYS);
  });

  it("rejects input that is not a number", () => {
    expect(parseCustomDays("")).toBeNull();
    expect(parseCustomDays("  ")).toBeNull();
    expect(parseCustomDays("abc")).toBeNull();
  });
});

describe("dimsForDays", () => {
  it("keeps the preset behaviour", () => {
    expect(dimsForDays(1)).toEqual(["daily"]);
    expect(dimsForDays(7)).toEqual(["daily"]);
    expect(dimsForDays(30)).toEqual(["daily", "weekly"]);
    expect(dimsForDays(90)).toEqual(["daily", "weekly"]);
    expect(dimsForDays(180)).toEqual(["weekly"]);
  });

  it("applies the same rule to a custom number of days", () => {
    expect(dimsForDays(2)).toEqual(["daily"]);
    expect(dimsForDays(14)).toEqual(["daily", "weekly"]);
    expect(dimsForDays(120)).toEqual(["weekly"]);
  });
});

describe("rangeLabel", () => {
  const t = (unit: "days" | "hours", n: number) => `${n}${unit === "days" ? "D" : "H"}`;
  it("reads one day as the rolling 24 hours", () => {
    expect(rangeLabel(1, t)).toBe("24H");
    expect(rangeLabel(30, t)).toBe("30D");
    expect(rangeLabel(3, t)).toBe("3D");
  });
});
