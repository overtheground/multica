"use client";

import type { ReactNode } from "react";
import { formatTokens } from "../../runtimes/utils";
import { useT } from "../../i18n";
import {
  formatShare,
  tokenBreakdownTotal,
  type TokenBreakdownRow,
} from "../utils";

export interface TokenBreakdownCardRow extends TokenBreakdownRow {
  label: ReactNode;
}

// One table shape for the "Models" and "Top tasks" cards: input, output,
// cache read, cache write, total, and the row's share of `shareTotal` (the
// whole window, so a truncated list still reads against the real total).
export function TokenBreakdownCard({
  title,
  nameHeader,
  rows,
  shareTotal,
  emptyLabel,
}: {
  title: string;
  nameHeader: string;
  rows: TokenBreakdownCardRow[];
  shareTotal: number;
  emptyLabel: string;
}) {
  const { t } = useT("usage");
  return (
    <div className="rounded-lg border bg-card">
      <div className="border-b px-4 pt-4 pb-3">
        <h4 className="text-body font-semibold">{title}</h4>
      </div>
      {rows.length === 0 ? (
        <p className="px-4 py-8 text-center text-caption text-muted-foreground">
          {emptyLabel}
        </p>
      ) : (
        <div
          role="region"
          aria-label={title}
          tabIndex={0}
          className="overflow-x-auto overscroll-x-contain"
        >
          <table className="w-full min-w-[40rem] text-caption tabular-nums">
            <thead>
              <tr className="border-b text-muted-foreground">
                <th className="px-4 py-2 text-left font-medium">{nameHeader}</th>
                <th className="px-2 py-2 text-right font-medium">
                  {t(($) => $.breakdown.input)}
                </th>
                <th className="px-2 py-2 text-right font-medium">
                  {t(($) => $.breakdown.output)}
                </th>
                <th className="px-2 py-2 text-right font-medium">
                  {t(($) => $.breakdown.cache_read)}
                </th>
                <th className="px-2 py-2 text-right font-medium">
                  {t(($) => $.breakdown.cache_write)}
                </th>
                <th className="px-2 py-2 text-right font-medium text-foreground">
                  {t(($) => $.breakdown.total)}
                </th>
                <th className="px-4 py-2 text-right font-medium">
                  {t(($) => $.breakdown.share)}
                </th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {rows.map((row) => (
                <tr key={row.key}>
                  <td className="max-w-0 px-4 py-2 text-left">
                    <div className="truncate text-body">{row.label}</div>
                  </td>
                  <td className="px-2 py-2 text-right">{formatTokens(row.input)}</td>
                  <td className="px-2 py-2 text-right">{formatTokens(row.output)}</td>
                  <td className="px-2 py-2 text-right">{formatTokens(row.cacheRead)}</td>
                  <td className="px-2 py-2 text-right">{formatTokens(row.cacheWrite)}</td>
                  <td className="px-2 py-2 text-right font-medium text-foreground">
                    {formatTokens(tokenBreakdownTotal(row))}
                  </td>
                  <td className="px-4 py-2 text-right text-muted-foreground">
                    {formatShare(tokenBreakdownTotal(row), shareTotal)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
