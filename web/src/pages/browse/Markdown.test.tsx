import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Markdown } from './Markdown';
import { safeHref, safeImageSrc } from './urlPolicy';

describe('Markdown', () => {
  it('renders common README structure', () => {
    const { container } = render(
      <Markdown
        source={[
          'Title',
          '=====',
          '',
          'Some *emphasis*, **strong**, `code` and ~~gone~~.',
          '',
          '- one',
          '- two',
          '  - nested',
          '',
          '1. first',
          '2. second',
          '',
          '> quoted',
          '',
          '```js',
          'const x = "<b>not html</b>";',
          '```',
          '',
          '| a | b |',
          '|---|--:|',
          '| 1 | 2 |',
          '',
          'See [docs][ref] and <https://example.com/auto>.',
          '',
          '[ref]: https://docs.example.com',
        ].join('\n')}
      />,
    );
    expect(screen.getByRole('heading', { name: 'Title' }).tagName).toBe('H3');
    expect(container.querySelector('em')).toHaveTextContent('emphasis');
    expect(container.querySelector('strong')).toHaveTextContent('strong');
    expect(container.querySelector('del')).toHaveTextContent('gone');
    expect(container.querySelectorAll('ul li')).toHaveLength(3);
    expect(container.querySelectorAll('ol > li')).toHaveLength(2);
    expect(container.querySelector('blockquote')).toHaveTextContent('quoted');
    expect(container.querySelector('pre code')).toHaveTextContent('const x = "<b>not html</b>";');
    expect(container.querySelector('pre b')).toBeNull();
    expect(screen.getByRole('table')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'docs' })).toHaveAttribute('href', 'https://docs.example.com/');
    expect(screen.getByRole('link', { name: 'https://example.com/auto' })).toBeInTheDocument();
  });

  it('never renders raw HTML, scripts or unsafe URLs', () => {
    const { container } = render(
      <Markdown
        source={[
          '<p align="center"><img src="https://img.example.com/logo.png" alt="logo"></p>',
          '<script>window.pwned = 1</script>',
          '<div onclick="steal()">text <b>bold</b></div>',
          '[js](javascript:alert(1)) [data](data:text/html,x) [rel](./docs/a.md) [anchor](#usage)',
          '![tracker](http://img.example.com/t.gif) ![ok](https://img.example.com/ok.png)',
          '<a href="https://example.com/a">html link</a>',
        ].join('\n\n')}
      />,
    );
    expect(container.querySelector('script, div[onclick], b, p[align]')).toBeNull();
    expect(container.textContent).not.toContain('pwned');
    expect(screen.getByText(/text bold/)).toBeInTheDocument();
    for (const name of ['js', 'data', 'rel']) expect(screen.queryByRole('link', { name })).toBeNull();
    expect(screen.getByRole('link', { name: 'anchor' })).toHaveAttribute('href', '#usage');
    expect(screen.getByRole('link', { name: 'html link' })).toHaveAttribute('target', '_blank');
    expect(screen.getByText('[image: tracker]')).toBeInTheDocument();
    const imgs = container.querySelectorAll('img');
    expect([...imgs].map((i) => i.getAttribute('alt'))).toEqual(['logo', 'ok']);
    for (const i of imgs) expect(i).toHaveAttribute('referrerpolicy', 'no-referrer');
  });

  it('allows only http(s), mailto and in-page links, and https images', () => {
    expect(safeHref('javascript:alert(1)')).toBeNull();
    expect(safeHref(' JAVASCRIPT:alert(1)')).toBeNull();
    expect(safeHref('vbscript:x')).toBeNull();
    expect(safeHref('mailto:a@example.com')).toBe('mailto:a@example.com');
    expect(safeHref('//evil.example.com')).toBeNull();
    expect(safeImageSrc('http://x.example.com/a.png')).toBeNull();
    expect(safeImageSrc('data:image/png;base64,AAAA')).toBeNull();
  });
});
