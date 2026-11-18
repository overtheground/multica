"use client";

import { useQuery } from "@tanstack/react-query";
import { chatSessionContextOptions } from "@multica/core/chat/queries";
import { formatTokens } from "../../runtimes/utils";
import { useT } from "../../i18n";
import {
  CONTEXT_LEVEL_CLASS,
  cacheShare,
  contextLevel,
  useContextThreshold,
} from "../../dashboard/context-utils";

/**
 * Compact "ctx 142k" next to the chat title: the context the model received on
 * the bot's latest answer (input + cache read + cache creation), not the chat
 * length. Refreshed after every answer via the chat:done event; colour follows
 * the shared threshold (200k by default).
 */
export function ChatContextBadge({ sessionId }: { sessionId: string }) {
  const { t } = useT("chat");
  const { data } = useQuery(chatSessionContextOptions(sessionId));
  const [threshold] = useContextThreshold();
  if (!data || data.context_tokens <= 0) return null;

  const level = contextLevel(data.context_tokens, threshold);
  const size = formatTokens(data.context_tokens);
  const cache = Math.round(cacheShare(data.context_tokens, data.cache_read_tokens) * 100);
  return (
    <span
      title={t(($) => $.context.title, { size: data.context_tokens.toLocaleString(), cache })}
      className={`shrink-0 rounded-sm px-1.5 py-0.5 text-caption tabular-nums ring-1 ring-border ${CONTEXT_LEVEL_CLASS[level]}`}
    >
      {t(($) => $.context.label, { size })}
    </span>
  );
}
