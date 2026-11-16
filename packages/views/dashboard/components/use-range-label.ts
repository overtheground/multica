"use client";

import { useT } from "../../i18n";
import { rangeLabel, type TimeRange } from "./dashboard-shared";

/** Formats a range as the page shows it: "24H" for the rolling day, "30D"
 *  otherwise, with the unit taken from the active locale. */
export function useRangeLabel(): (days: TimeRange) => string {
  const { t } = useT("usage");
  return (days) =>
    rangeLabel(days, (unit, n) =>
      unit === "hours"
        ? t(($) => $.filter.range_hours, { n })
        : t(($) => $.filter.range_days, { n }),
    );
}
