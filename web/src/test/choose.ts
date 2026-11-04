import type { UserEvent } from '@testing-library/user-event';

/**
 * Picks an option in a custom Select: opens it from its trigger and clicks the
 * option whose value (or visible label) matches, like `user.selectOptions`.
 */
export async function choose(user: UserEvent, trigger: HTMLElement, value: string) {
  await user.click(trigger);
  const list = document.getElementById(trigger.getAttribute('aria-controls') ?? '');
  if (!list) throw new Error('select did not open');
  const options = [...list.querySelectorAll<HTMLElement>('[role="option"]')];
  const option = options.find((o) => o.dataset.value === value) ?? options.find((o) => o.textContent?.trim() === value);
  if (!option) throw new Error(`no option ${JSON.stringify(value)} in [${options.map((o) => o.dataset.value).join(', ')}]`);
  await user.click(option);
}

/** The value a custom Select currently holds. */
export const valueOf = (trigger: HTMLElement) => trigger.getAttribute('data-value');
