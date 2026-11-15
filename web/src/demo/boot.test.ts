import { openHashLink } from './boot';

function fakeWindow(hash: string) {
  const replaceState = vi.fn();
  return { loc: { hash } as Location, history: { replaceState } as unknown as History, replaceState };
}

describe('openHashLink', () => {
  it('turns a hash link into the path under the base', () => {
    const w = fakeWindow('#/repositories?type=proxy');
    openHashLink('/hangar/demo/', w.loc, w.history);
    expect(w.replaceState).toHaveBeenCalledWith(null, '', '/hangar/demo/repositories?type=proxy');
  });

  it('opens the overview for "#/"', () => {
    const w = fakeWindow('#/');
    openHashLink('/hangar/demo/', w.loc, w.history);
    expect(w.replaceState).toHaveBeenCalledWith(null, '', '/hangar/demo/');
  });

  it('leaves in-page anchors alone', () => {
    const w = fakeWindow('#endpoints-title');
    openHashLink('/hangar/demo/', w.loc, w.history);
    expect(w.replaceState).not.toHaveBeenCalled();
  });
});
