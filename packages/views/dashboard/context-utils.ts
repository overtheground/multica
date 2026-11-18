import { useCallback, useEffect, useState } from "react";
import type { DashboardContextRow } from "@multica/core/types";

// Context-size monitoring (MUL-197). "Context" is what the model received on a
// call: input + cache read + cache creation tokens.

export const DEFAULT_CONTEXT_THRESHOLD = 200_000;
const THRESHOLD_STORAGE_KEY = "multica:context-threshold";
// Colour steps: amber from this share of the threshold, red at the threshold.
const WARN_RATIO = 0.75;

export type ContextLevel = "ok" | "warn" | "over";

export function contextLevel(tokens: number, threshold: number): ContextLevel {
  if (threshold <= 0) return "ok";
  if (tokens >= threshold) return "over";
  if (tokens >= threshold * WARN_RATIO) return "warn";
  return "ok";
}

export const CONTEXT_LEVEL_CLASS: Record<ContextLevel, string> = {
  ok: "text-muted-foreground",
  warn: "text-amber-600 dark:text-amber-400",
  over: "text-destructive",
};

/** Mean context per model call; 0 when no calls were recorded. */
export function averageContext(row: Pick<DashboardContextRow, "sum_tokens" | "calls">): number {
  return row.calls > 0 ? Math.round(row.sum_tokens / row.calls) : 0;
}

/** Share of the context that came from the prompt cache, 0..1. */
export function cacheShare(contextTokens: number, cacheReadTokens: number): number {
  return contextTokens > 0 ? Math.min(1, cacheReadTokens / contextTokens) : 0;
}

export function parseThreshold(raw: string | null): number {
  const n = Number(raw);
  return Number.isFinite(n) && n >= 1000 ? Math.round(n) : DEFAULT_CONTEXT_THRESHOLD;
}

/** Highlight threshold in tokens, kept in localStorage and shared across tabs. */
export function useContextThreshold(): [number, (tokens: number) => void] {
  const [threshold, setThreshold] = useState(DEFAULT_CONTEXT_THRESHOLD);
  useEffect(() => {
    const read = () => setThreshold(parseThreshold(localStorage.getItem(THRESHOLD_STORAGE_KEY)));
    read();
    window.addEventListener("storage", read);
    window.addEventListener("multica:context-threshold", read);
    return () => {
      window.removeEventListener("storage", read);
      window.removeEventListener("multica:context-threshold", read);
    };
  }, []);
  const update = useCallback((tokens: number) => {
    localStorage.setItem(THRESHOLD_STORAGE_KEY, String(parseThreshold(String(tokens))));
    window.dispatchEvent(new Event("multica:context-threshold"));
  }, []);
  return [threshold, update];
}
