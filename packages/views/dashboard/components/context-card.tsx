"use client";

import { useQuery } from "@tanstack/react-query";
import { dashboardContextOptions } from "@multica/core/dashboard/queries";
import type { DashboardContextRow } from "@multica/core/types";
import { formatTokens } from "../../runtimes/utils";
import { useT } from "../../i18n";
import {
  CONTEXT_LEVEL_CLASS,
  averageContext,
  contextLevel,
  parseThreshold,
  useContextThreshold,
} from "../context-utils";
import { bucketKey, isSyntheticAgentRow } from "../utils";

// Peak and average context per agent plus a per-day peak chart, with
// everything at or above the threshold highlighted (MUL-197).
export function ContextCard({
  wsId,
  days,
  projectId,
  tz,
  agents,
}: {
  wsId: string;
  days: number;
  projectId: string | null;
  tz: string;
  agents: { id: string; name: string }[];
}) {
  const { t } = useT("usage");
  const [threshold, setThreshold] = useContextThreshold();
  const byAgent = useQuery(dashboardContextOptions("by-agent", wsId, days, projectId, tz));
  const daily = useQuery(dashboardContextOptions("daily", wsId, days, projectId, tz));
  const agentRows = byAgent.data ?? [];
  const dailyRows = (daily.data ?? []).toSorted((a, b) =>
    bucketKey({ date: a.date ?? "", hour: a.hour }).localeCompare(
      bucketKey({ date: b.date ?? "", hour: b.hour }),
    ),
  );
  const chartMax = Math.max(threshold, ...dailyRows.map((r) => r.peak_tokens));

  const nameOf = (row: DashboardContextRow) => {
    const id = row.agent_id ?? "";
    if (isSyntheticAgentRow(id)) return t(($) => $.context.other_agents);
    return agents.find((a) => a.id === id)?.name ?? id.slice(0, 8);
  };

  return (
    <div className="rounded-lg border bg-card">
      <div className="flex items-center justify-between gap-3 border-b px-4 pt-4 pb-3">
        <h4 className="text-body font-semibold">{t(($) => $.context.title)}</h4>
        <label className="flex items-center gap-2 text-caption text-muted-foreground">
          {t(($) => $.context.threshold)}
          <input
            type="number"
            min={1}
            step={10}
            key={threshold}
            defaultValue={Math.round(threshold / 1000)}
            aria-label={t(($) => $.context.threshold)}
            onBlur={(e) => setThreshold(parseThreshold(String(Number(e.currentTarget.value) * 1000)))}
            className="w-16 rounded-sm bg-background px-1 py-0.5 text-right tabular-nums ring-1 ring-border"
          />
        </label>
      </div>
      {agentRows.length === 0 ? (
        <p className="px-4 py-8 text-center text-caption text-muted-foreground">
          {t(($) => $.context.no_data)}
        </p>
      ) : (
        <>
          {dailyRows.length > 0 && (
            <div
              role="img"
              aria-label={t(($) => $.context.chart_label)}
              className="relative mx-4 mt-3 flex h-24 items-end gap-0.5 border-b"
            >
              <div
                className="absolute inset-x-0 border-t border-dashed border-destructive/60"
                style={{ bottom: `${(threshold / chartMax) * 100}%` }}
              />
              {dailyRows.map((r) => {
                const key = bucketKey({ date: r.date ?? "", hour: r.hour });
                const level = contextLevel(r.peak_tokens, threshold);
                return (
                  <div
                    key={key}
                    title={`${key} · ${t(($) => $.context.peak)} ${formatTokens(r.peak_tokens)} · ${t(($) => $.context.average)} ${formatTokens(averageContext(r))}`}
                    className={`min-w-px flex-1 rounded-t-sm bg-current ${CONTEXT_LEVEL_CLASS[level]} ${level === "ok" ? "opacity-40" : ""}`}
                    style={{ height: `${Math.max(2, (r.peak_tokens / chartMax) * 100)}%` }}
                  />
                );
              })}
            </div>
          )}
          <div role="region" aria-label={t(($) => $.context.title)} tabIndex={0} className="overflow-x-auto">
            <table className="w-full min-w-[30rem] text-caption tabular-nums">
              <thead>
                <tr className="border-b text-muted-foreground">
                  <th className="px-4 py-2 text-left font-medium">{t(($) => $.context.agent)}</th>
                  <th className="px-2 py-2 text-right font-medium">{t(($) => $.context.peak)}</th>
                  <th className="px-2 py-2 text-right font-medium">{t(($) => $.context.average)}</th>
                  <th className="px-2 py-2 text-right font-medium">{t(($) => $.context.calls)}</th>
                  <th className="px-4 py-2 text-right font-medium">{t(($) => $.context.runs)}</th>
                </tr>
              </thead>
              <tbody className="divide-y">
                {agentRows.map((row) => (
                  <tr key={row.agent_id}>
                    <td className="max-w-0 truncate px-4 py-2 text-left text-body">{nameOf(row)}</td>
                    <td
                      className={`px-2 py-2 text-right font-medium ${CONTEXT_LEVEL_CLASS[contextLevel(row.peak_tokens, threshold)]}`}
                    >
                      {formatTokens(row.peak_tokens)}
                    </td>
                    <td
                      className={`px-2 py-2 text-right ${CONTEXT_LEVEL_CLASS[contextLevel(averageContext(row), threshold)]}`}
                    >
                      {formatTokens(averageContext(row))}
                    </td>
                    <td className="px-2 py-2 text-right">{row.calls.toLocaleString()}</td>
                    <td className="px-4 py-2 text-right">{row.task_count.toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}
