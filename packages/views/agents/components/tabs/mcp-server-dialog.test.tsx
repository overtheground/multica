// @vitest-environment jsdom

import type { ComponentProps, ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../../locales/en/common.json";
import enAgents from "../../../locales/en/agents.json";
import type { ManagedMcpServer } from "./mcp-config-model";
import { McpServerDialog } from "./mcp-server-dialog";

const TEST_RESOURCES = { en: { common: enCommon, agents: enAgents } };

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function managedServer(overrides: Partial<ManagedMcpServer> = {}): ManagedMcpServer {
  return {
    name: "fetch",
    config: { command: "uvx" },
    container: "mcpServers",
    transport: "stdio",
    enabled: true,
    ...overrides,
  };
}

function renderRevealedDialog(overrides: Partial<ComponentProps<typeof McpServerDialog>> = {}) {
  const onSave = vi.fn().mockResolvedValue(undefined);
  const onOpenChange = vi.fn();
  const props = {
    open: true,
    server: null,
    existingNames: new Set<string>(),
    onSave,
    onOpenChange,
    ...overrides,
  };
  const view = render(<McpServerDialog {...props} />, { wrapper: Wrapper });
  const reveal = screen.queryByRole("button", { name: "Show configuration" });
  if (reveal) fireEvent.click(reveal);
  return { ...view, onSave: props.onSave, onOpenChange: props.onOpenChange };
}

describe("McpServerDialog", () => {
  it.each(["context7.dev", "legacy server", " padded "])(
    "edits configuration while preserving the historical name %s exactly",
    async (name) => {
      const user = userEvent.setup();
      const { onSave, onOpenChange } = renderRevealedDialog({
        server: managedServer({ name }),
        existingNames: new Set([name]),
        hideNameWhenEditing: true,
      });

      expect(screen.queryByLabelText("Server name")).toBeNull();
      await user.clear(screen.getByLabelText("Command"));
      await user.type(screen.getByLabelText("Command"), "updated-command");
      await user.click(screen.getByRole("button", { name: "Save" }));

      await waitFor(() =>
        expect(onSave).toHaveBeenCalledWith(name, { command: "updated-command" }),
      );
      expect(onOpenChange).toHaveBeenCalledWith(false);
    },
  );

  it.each(["create", "rename"])(
    "rejects a newly entered historical-style name during %s",
    async (action) => {
      const user = userEvent.setup();
      const { onSave, onOpenChange } = renderRevealedDialog({
        server: action === "rename" ? managedServer() : null,
      });

      const input = screen.getByLabelText("Server name");
      await user.clear(input);
      await user.type(input, "context7.dev");
      if (action === "create") {
        await user.type(screen.getByLabelText("Command"), "uvx");
      }
      await user.click(screen.getByRole("button", {
        name: action === "create" ? "Add" : "Save",
      }));

      expect(input).toHaveFocus();
      expect(input).toHaveAttribute("aria-invalid", "true");
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Use only letters, numbers, hyphens, and underscores.",
      );
      expect(onSave).not.toHaveBeenCalled();
      expect(onOpenChange).not.toHaveBeenCalled();
    },
  );

  it.each([
    ["sse", "STDIO", "Command", "uvx", { command: "uvx" }],
    ["websocket", "Streamable HTTP", "Server URL", "https://new.example/mcp", {
      type: "http", url: "https://new.example/mcp",
    }],
  ])(
    "replaces a %s server through the visual editor using %s",
    async (transport, selected, label, value, expected) => {
      const user = userEvent.setup();
      const { onSave } = renderRevealedDialog({
        server: managedServer({ transport, config: {} }),
        replacementMode: true,
      });

      expect(screen.getByRole("tab", { name: "JSON" })).toHaveAttribute(
        "aria-selected", "true",
      );
      await user.click(screen.getByRole("tab", { name: "Visual editor" }));
    const reveal = screen.queryByRole("button", { name: "Show configuration" });
    if (reveal) await user.click(reveal);
      await user.click(screen.getByRole("button", { name: new RegExp(`^${selected}`) }));
      await user.type(screen.getByLabelText(label), value);
      await user.click(screen.getByRole("button", { name: "Replace configuration" }));

      expect(onSave).toHaveBeenCalledWith("fetch", expected);
    },
  );

  it("blocks duplicate submits and dismissal until a save completes", async () => {
    const user = userEvent.setup();
    let finishSave!: () => void;
    const onSave = vi.fn(() => new Promise<void>((resolve) => { finishSave = resolve; }));
    const { onOpenChange } = renderRevealedDialog({ server: managedServer(), onSave });

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    fireEvent.submit(screen.getByLabelText("Command").closest("form")!);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByRole("dialog")).toBeVisible();
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(onSave).toHaveBeenCalledExactlyOnceWith("fetch", { command: "uvx" });

    finishSave();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it.each(["Cancel", "Close", "Escape"])(
    "discards an unsaved draft when dismissed with %s",
    async (action) => {
      const user = userEvent.setup();
      const { onSave, onOpenChange } = renderRevealedDialog();
      await user.type(screen.getByLabelText("Server name"), "draft");

      if (action === "Escape") await user.keyboard("{Escape}");
      else await user.click(screen.getByRole("button", { name: action }));

      expect(onOpenChange).toHaveBeenCalledWith(false);
      expect(onSave).not.toHaveBeenCalled();
    },
  );

  it("clears submitted JSON errors when returning to the visual editor", async () => {
    const user = userEvent.setup();
    const { onSave } = renderRevealedDialog();
    await user.type(screen.getByLabelText("Server name"), "fetch");
    await user.type(screen.getByLabelText("Command"), "uvx");
    await user.click(screen.getByRole("tab", { name: "JSON" }));
    await user.click(screen.getByRole("button", { name: "Show configuration" }));
    fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), {
      target: { value: "{invalid" },
    });
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Invalid JSON");

    await user.click(screen.getByRole("tab", { name: "Visual editor" }));
    const reveal = screen.queryByRole("button", { name: "Show configuration" });
    if (reveal) await user.click(reveal);

    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByLabelText("Command")).toHaveValue("uvx");
    expect(screen.getByLabelText("Command")).not.toHaveAttribute("aria-invalid");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(onSave).toHaveBeenCalledWith("fetch", { command: "uvx" });
  });

  it("removes one argument and environment row while preserving their siblings", async () => {
    const user = userEvent.setup();
    const { onSave } = renderRevealedDialog({
      server: managedServer({
        config: {
          command: "uvx",
          args: ["first", "remove", "last"],
          env: { FIRST: "one", REMOVE: "two", LAST: "three" },
        },
      }),
    });

    await user.click(screen.getByRole("button", { name: "Remove argument 2" }));
    await user.clear(screen.getByLabelText("Startup arguments 2"));
    await user.type(screen.getByLabelText("Startup arguments 2"), "updated-last");
    await user.click(screen.getByRole("button", { name: "Remove environment variable 2" }));
    await user.clear(screen.getByLabelText("Environment variables: Variable name 2"));
    await user.type(screen.getByLabelText("Environment variables: Variable name 2"), "UPDATED");
    await user.clear(screen.getByLabelText("Environment variables: Value 2"));
    await user.type(screen.getByLabelText("Environment variables: Value 2"), "four");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSave).toHaveBeenCalledWith("fetch", {
      command: "uvx",
      args: ["first", "updated-last"],
      env: { FIRST: "one", UPDATED: "four" },
    });
  });

  it("removes one HTTP header while preserving and editing its siblings", async () => {
    const user = userEvent.setup();
    const { onSave } = renderRevealedDialog({
      server: managedServer({
        transport: "http",
        config: {
          type: "http",
          url: "https://example.test/mcp",
          headers: { First: "one", Remove: "two", Last: "three" },
        },
      }),
    });

    await user.click(screen.getByRole("button", { name: "Remove header 2" }));
    await user.clear(screen.getByLabelText("HTTP headers: Header name 2"));
    await user.type(screen.getByLabelText("HTTP headers: Header name 2"), "Updated");
    await user.clear(screen.getByLabelText("HTTP headers: Value 2"));
    await user.type(screen.getByLabelText("HTTP headers: Value 2"), "four");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSave).toHaveBeenCalledWith("fetch", {
      type: "http",
      url: "https://example.test/mcp",
      headers: { First: "one", Updated: "four" },
    });
  });
});


describe("concealed MCP configuration", () => {
  const server = managedServer({ config: { type: "http", url: "https://mcp.example/secret-url", headers: { Authorization: "Bearer sensitive-token" }, extra: "preserved" }, transport: "http" });

  it("does not render JSON or visual values before reveal; conceals each newly opened tab", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn();
    const props = { open: true, server, initialMode: "json" as const, existingNames: new Set<string>(), onSave, onOpenChange: vi.fn() };
    const view = render(<McpServerDialog {...props} />, { wrapper: Wrapper });
    expect(screen.queryByLabelText("MCP server JSON configuration")).toBeNull();
    expect(document.body.innerHTML).not.toContain("sensitive-token");
    expect(document.body.innerHTML).not.toContain("secret-url");
    await user.click(screen.getByRole("button", { name: "Show configuration" }));
    expect(screen.getByLabelText("MCP server JSON configuration")).toHaveValue(JSON.stringify(server.config, null, 2));
    await user.click(screen.getByRole("button", { name: "Hide configuration" }));
    expect(screen.queryByLabelText("MCP server JSON configuration")).toBeNull();
    expect(document.body.innerHTML).not.toContain("sensitive-token");
    await user.click(screen.getByRole("tab", { name: "Visual editor" }));
    expect(screen.queryByLabelText("Server URL")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Show configuration" }));
    expect(screen.getByLabelText("Server URL")).toHaveValue("https://mcp.example/secret-url");
    await user.click(screen.getByRole("tab", { name: "JSON" }));
    expect(screen.queryByLabelText("MCP server JSON configuration")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Show configuration" }));
    view.rerender(<McpServerDialog {...props} open={false} />);
    view.rerender(<McpServerDialog {...props} />);
    expect(screen.queryByLabelText("MCP server JSON configuration")).toBeNull();
    expect(document.body.innerHTML).not.toContain("sensitive-token");
  });

  it("keeps an edited draft when hidden and does not include parser fragments in errors", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<McpServerDialog open server={server} initialMode="json" existingNames={new Set()} onSave={onSave} onOpenChange={vi.fn()} />, { wrapper: Wrapper });
    await user.click(screen.getByRole("button", { name: "Show configuration" }));
    const draft = JSON.stringify({ ...server.config, custom: "new-secret" });
    fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), { target: { value: draft } });
    await user.click(screen.getByRole("button", { name: "Hide configuration" }));
    await user.click(screen.getByRole("button", { name: "Show configuration" }));
    expect(screen.getByLabelText("MCP server JSON configuration")).toHaveValue(draft);
    fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), { target: { value: '{"secret":"sensitive-token", broken}' } });
    await user.click(screen.getByRole("button", { name: "Hide configuration" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByRole("alert")).not.toHaveTextContent("sensitive-token");
    expect(onSave).not.toHaveBeenCalled();
  });

  it("can keep the saved configuration intact without revealing it", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<McpServerDialog open server={server} initialMode="json" existingNames={new Set()} onSave={onSave} onOpenChange={vi.fn()} />, { wrapper: Wrapper });
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave).toHaveBeenCalledWith(server.name, server.config);
  });
});
