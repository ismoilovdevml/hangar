import { getDomain } from './tldts';

describe('demo getDomain', () => {
  it('answers the last two labels of a host name', () => {
    expect(getDomain('ismoilovdevml.github.io')).toBe('github.io');
    expect(getDomain('pkg.acme.io.')).toBe('acme.io');
  });

  it('has no registrable domain for single labels and addresses', () => {
    expect(getDomain('localhost')).toBeNull();
    expect(getDomain('127.0.0.1')).toBeNull();
    expect(getDomain('::1')).toBeNull();
  });
});
