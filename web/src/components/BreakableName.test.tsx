import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { BreakableName } from './BreakableName';

describe('BreakableName', () => {
  it.each([
    ['System.Security.Cryptography.Xml', 3],
    ['@acme/ui-kit', 1],
    ['com.acme:ledger-core', 2],
    ['payments-api', 0],
    ['.hidden', 0],
  ])('lets %s wrap after its separators only', (name, breaks) => {
    const { container } = render(
      <span>
        <BreakableName name={name} />
      </span>,
    );
    expect(container.querySelectorAll('wbr')).toHaveLength(breaks);
    expect(container.textContent).toBe(name);
  });
});
