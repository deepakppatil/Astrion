import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import InteractionCard from "../components/InteractionCard";
import type { Interaction } from "../lib/api";

const base: Interaction = {
  id: "int_1", jobId: "job_1", executionId: "exe_1", stepId: "clarify", kind: "MULTIPLE_CHOICE",
  prompt: "Which database?", options: [{ value: "pg", label: "PostgreSQL" }, { value: "my", label: "MySQL" }],
  schema: { required: true }, status: "OPEN", createdAt: new Date().toISOString(), openedAt: new Date().toISOString(), canRespond: true,
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("InteractionCard", () => {
  it("submits the selected option with an idempotency key", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ ...base, status: "RESOLVED" }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = vi.fn();
    render(<InteractionCard it={base} onResolved={onResolved} />);
    const submit = screen.getByRole("button", { name: "Submit answer" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByLabelText(/PostgreSQL/));
    fireEvent.click(submit);
    await vi.waitFor(() => expect(onResolved).toHaveBeenCalled());
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/interactions/int_1/responses");
    expect(JSON.parse(init.body)).toMatchObject({ values: ["pg"] });
    expect(init.headers["Idempotency-Key"]).toBeTruthy();
  });

  it("shows approve and reject for decision interactions", () => {
    render(<InteractionCard it={{ ...base, kind: "APPROVAL", options: [], platform: true, productionApproval: true }} />);
    expect(screen.getByRole("button", { name: "Approve" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reject" })).toBeTruthy();
    expect(screen.getByText("production")).toBeTruthy();
  });

  it("hides controls when the user may not respond", () => {
    render(<InteractionCard it={{ ...base, canRespond: false }} />);
    expect(screen.queryByRole("button", { name: "Submit answer" })).toBeNull();
    expect(screen.getByText(/not an authorized responder/)).toBeTruthy();
  });

  it("surfaces server validation errors", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: { code: "expired", message: "interaction expired" } }), { status: 410 })));
    render(<InteractionCard it={{ ...base, kind: "FREE_TEXT", options: [] }} />);
    fireEvent.change(screen.getByPlaceholderText("Your answer"), { target: { value: "hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit answer" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("interaction expired");
  });
});
