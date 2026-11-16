"use client";

import {
  NumberFlow,
  NumberFlowGroup,
} from "@multica/ui/components/ui/number-flow";
import { formatDuration } from "../utils";

// Period selector — presets plus a free-form number of days. `dims` on a
// preset documents which chart dimensions the range may be drawn at (see
// `dimsForDays`): 7d at the weekly grain collapses to a single bar, 180d at the
// daily grain is 180 unreadable bars.
//
// 1d semantic: the rolling LAST 24 HOURS from now, drawn per hour — not the
// calendar day. The client asks the server for `hours=24`
// (setDashboardWindow in core/api/client.ts); the server cuts the window on
// the current hour edge and returns hourly buckets, so there is no empty tail
// after midnight.
export const TIME_RANGES = [
  { days: 1 },
  { days: 7 },
  { days: 30 },
  { days: 90 },
  { days: 180 },
] as const;

/** Longest selectable range. Keeps the weekly over-fetch
 *  (`ceil(days / 7) * 7`) inside the server's 365-day `days` cap. */
export const MAX_RANGE_DAYS = 180;

/** Number of days the dashboard is scoped to; 1 means the rolling 24 hours. */
export type TimeRange = number;
export type Dim = "daily" | "weekly";

/** Clamps free-form input to a whole number of days in 1..MAX_RANGE_DAYS.
 *  Returns null for anything that is not a number, so the caller can ignore it. */
export function parseCustomDays(raw: string): TimeRange | null {
  const n = Math.floor(Number(raw.trim()));
  if (raw.trim() === "" || !Number.isFinite(n)) return null;
  return Math.min(MAX_RANGE_DAYS, Math.max(1, n));
}

/**
 * Which chart dimensions the current range may be drawn at.
 *
 * The constraint between range and dimension used to be enforced in both
 * directions by two sibling controls in the page header: picking Weekly while
 * on 1d silently reset the range to 90d, which moved every KPI on the page.
 * The range is now the page-scoped filter and the dimension is card-scoped, so
 * the dependency runs one way only — a card offers whichever dimensions its
 * range allows, and nothing resets. A card-scoped control must never reach up
 * and change a page-scoped one (MUL-5759).
 *
 * Any number of days gets the same rule the presets follow: up to a week is
 * daily only, up to 90 days either, beyond that weekly only.
 */
export function dimsForDays(days: TimeRange): readonly Dim[] {
  if (days <= 7) return ["daily"];
  if (days <= 90) return ["daily", "weekly"];
  return ["weekly"];
}

/** Sentinel for "no project filter" — kept distinct from the empty string so
 *  it survives a refactor that ever lets a project be slug-keyed. */
export const ALL_PROJECTS = "__all__";

/**
 * Card-local segmented control — the pill toggle used *inside* a card to pick
 * what that card shows. The page header deliberately uses outline Buttons
 * instead, so the shape of a control tells you its scope: pills change one
 * card, buttons change the page.
 *
 * shadcn's Tabs is wired for full tab pages with ARIA semantics the compact
 * toolbar pill doesn't need. Which option is active was expressed only as a
 * colour swap, which no screen reader can see, so `aria-pressed` carries it
 * too. `label` is required rather than optional because a naked group of
 * toggle buttons is announced without saying WHAT it toggles — "Rate, pressed"
 * is useless until you know the group is the offender ranking. Toggle buttons
 * rather than a radiogroup: a radiogroup owes the user arrow-key roving focus,
 * and these are tab stops wherever they appear in the page.
 */
export function Segmented<T extends string | number>({
  value,
  onChange,
  options,
  label,
}: {
  value: T;
  onChange: (v: T) => void;
  options: readonly { label: string; value: T }[];
  label: string;
}) {
  return (
    <div
      role="group"
      aria-label={label}
      className="inline-flex items-center gap-0.5 rounded-md bg-muted p-0.5"
    >
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          aria-pressed={o.value === value}
          onClick={() => onChange(o.value)}
          className={`rounded-sm px-2.5 py-1 text-caption font-medium transition-colors ${
            o.value === value
              ? "bg-background text-foreground shadow-sm"
              : "text-muted-foreground hover:text-foreground"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function DurationNumberFlow({
  seconds,
  lessThanMinuteLabel,
  locales,
}: {
  seconds: number;
  lessThanMinuteLabel: string;
  locales?: Intl.LocalesArgument;
}) {
  const label = formatDuration(seconds, lessThanMinuteLabel);
  const parts = Array.from(label.matchAll(/(\d+)([a-z]+)/gi), (match) => ({
    value: Number(match[1]),
    unit: match[2] ?? "",
  }));

  if (parts.length === 0) return label;

  return (
    <>
      <span className="sr-only">{label}</span>
      <NumberFlowGroup>
        <span aria-hidden className="inline-flex items-baseline gap-1">
          {parts.map((part) => (
            <NumberFlow
              key={part.unit}
              value={part.value}
              locales={locales}
              suffix={part.unit}
              format={{ maximumFractionDigits: 0, useGrouping: false }}
            />
          ))}
        </span>
      </NumberFlowGroup>
    </>
  );
}

/** Label of a range as the header and the KPI tiles show it: "24H" for the
 *  rolling day, "30D" otherwise (unit localised by the `range_*` keys). */
export function rangeLabel(
  days: TimeRange,
  t: (key: "days" | "hours", n: number) => string,
): string {
  return days === 1 ? t("hours", 24) : t("days", days);
}
