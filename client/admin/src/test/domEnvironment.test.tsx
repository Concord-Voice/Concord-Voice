import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

describe("admin DOM test environment", () => {
  it("keeps each successive keyboard focus target focus-visible", () => {
    render(
      <>
        <button>First</button>
        <button>Second</button>
        <button>Third</button>
      </>,
    );
    const first = screen.getByRole("button", { name: "First" });
    const second = screen.getByRole("button", { name: "Second" });
    const third = screen.getByRole("button", { name: "Third" });
    first.focus();

    const focusVisible: boolean[] = [];
    for (const [from, to] of [
      [first, second],
      [second, third],
      [third, first],
    ] as const) {
      from.dispatchEvent(
        new window.KeyboardEvent("keydown", { bubbles: true, key: "Tab" }),
      );
      to.focus();
      expect(document.activeElement).toBe(to);
      focusVisible.push(to.matches(":focus-visible"));
      to.dispatchEvent(
        new window.KeyboardEvent("keyup", { bubbles: true, key: "Tab" }),
      );
    }

    expect(focusVisible).toEqual([true, true, true]);
  });

  it("returns no element for an absent attribute and rejects malformed selectors", () => {
    render(<button>Existing</button>);

    expect(document.querySelector("[data-primary-home='missing']")).toBeNull();
    expect(() => document.querySelector("[data-primary-home=]")).toThrow(
      DOMException,
    );
    expect(() => document.querySelector("button >> span")).toThrow(
      DOMException,
    );
  });
});
