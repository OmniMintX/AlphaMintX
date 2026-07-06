// @vitest-environment jsdom

// Shared list UI: Pager boundary states + callbacks, ErrorBanner a11y role,
// StateBadge tone class. Rendered bare — useI18n's default context is the
// "en" catalog, no provider needed.

import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ErrorBanner, Pager, StateBadge } from "./ui";

afterEach(cleanup);

describe("Pager", () => {
  it("disables prev on page 1 and enables next when more pages exist", () => {
    render(<Pager page={1} total={50} limit={20} onPage={() => {}} />);
    expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Next page" })).toBeEnabled();
  });

  it("disables next on the last page", () => {
    render(<Pager page={3} total={50} limit={20} onPage={() => {}} />);
    expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Previous page" })).toBeEnabled();
  });

  it("calls onPage with the adjacent page on next/prev clicks", async () => {
    const user = userEvent.setup();
    const onPage = vi.fn();
    render(<Pager page={2} total={50} limit={20} onPage={onPage} />);
    await user.click(screen.getByRole("button", { name: "Next page" }));
    expect(onPage).toHaveBeenCalledWith(3);
    await user.click(screen.getByRole("button", { name: "Previous page" }));
    expect(onPage).toHaveBeenCalledWith(1);
  });
});

describe("ErrorBanner", () => {
  it("renders the message under role=alert", () => {
    render(<ErrorBanner message="boom" />);
    expect(screen.getByRole("alert")).toHaveTextContent("boom");
  });
});

describe("StateBadge", () => {
  it("renders the draft state with its neutral tone", () => {
    const { container } = render(<StateBadge state="draft" />);
    expect(screen.getByText("Draft")).toBeInTheDocument();
    expect(container.querySelector(".badge-neutral")).not.toBeNull();
  });
});
