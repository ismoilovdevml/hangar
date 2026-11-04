import { fireEvent, render, screen, within } from '@testing-library/react';
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
    // Without a source from the server, images are their text; never a direct load.
    expect(container.querySelectorAll('img')).toHaveLength(0);
    for (const alt of ['logo', 'tracker', 'ok']) expect(screen.getByRole('img', { name: alt })).toHaveClass('md-img-chip');
  });

  it('loads images from the sources the server resolved, inline, linked and bounded', () => {
    const images = {
      'https://ci.example.com/badge.svg': '/api/v1/components/c1/readme-images/s1?u=https%3A%2F%2Fci.example.com%2Fbadge.svg',
      'http://img.example.com/nuget.svg?a=1&b=2': '/api/v1/components/c1/readme-images/s2?u=x',
      'images/logo.png': '/api/v1/components/c1/readme-images/s3?p=images%2Flogo.png',
      'https://cdn.example.com/direct.png': 'https://cdn.example.com/direct.png',
    };
    const { container } = render(
      <Markdown
        images={images}
        source={[
          '<p align="center"><img src="images/logo.png" alt="Logo" width="200px" height="80%"></p>',
          '',
          '[![CI](https://ci.example.com/badge.svg)](https://ci.example.com)',
          '<a href="https://nuget.example.com"><img alt="NuGet" src="http://img.example.com/nuget.svg?a=1&amp;b=2"></a>',
          '![direct](https://cdn.example.com/direct.png) ![inline data](data:image/png;base64,AAAA) ![html](data:text/html,x)',
        ].join('\n')}
      />,
    );
    const logo = screen.getByRole('img', { name: 'Logo' });
    expect(logo).toHaveAttribute('src', images['images/logo.png']);
    expect(logo).toHaveAttribute('width', '200');
    expect(logo).not.toHaveAttribute('height');
    // The badge row stays one paragraph, each badge clickable.
    const ci = screen.getByRole('img', { name: 'CI' });
    const nuget = screen.getByRole('img', { name: 'NuGet' });
    expect(ci.closest('p')).toBe(nuget.closest('p'));
    expect(ci.closest('a')).toHaveAttribute('href', 'https://ci.example.com/');
    expect(nuget.closest('a')).toHaveAttribute('href', 'https://nuget.example.com/');
    expect(nuget).toHaveAttribute('src', images['http://img.example.com/nuget.svg?a=1&b=2']);
    expect(screen.getByRole('img', { name: 'direct' })).toHaveAttribute('src', 'https://cdn.example.com/direct.png');
    expect(screen.getByRole('img', { name: 'inline data' })).toHaveAttribute('src', 'data:image/png;base64,AAAA');
    expect(screen.getByRole('img', { name: 'html' })).toHaveClass('md-img-chip');
    for (const img of container.querySelectorAll('img')) {
      expect(img).toHaveAttribute('loading', 'lazy');
      expect(img).toHaveAttribute('referrerpolicy', 'no-referrer');
      expect(img).toHaveClass('md-img');
    }
  });

  it('turns an image that fails to load into a chip with its text, keeping the link', () => {
    render(<Markdown images={{ 'https://x.example.com/b.svg': '/api/v1/components/c1/readme-images/s?u=b' }} source="[![Docs **status**](https://x.example.com/b.svg)](https://docs.example.com)" />);
    const img = screen.getByRole('img', { name: 'Docs status' });
    expect(img.tagName).toBe('IMG');
    fireEvent.error(img);
    const chip = screen.getByRole('img', { name: 'Docs status' });
    expect(chip.tagName).toBe('SPAN');
    expect(chip).toHaveAttribute('title', 'Image could not be loaded: Docs status');
    expect(within(screen.getByRole('link')).getByText('Docs status')).toBeInTheDocument();
    expect(screen.queryByText(/\[image/)).toBeNull();
  });

  it('allows only http(s), mailto and in-page links, and server-resolved, https or data images', () => {
    expect(safeHref('javascript:alert(1)')).toBeNull();
    expect(safeHref(' JAVASCRIPT:alert(1)')).toBeNull();
    expect(safeHref('vbscript:x')).toBeNull();
    expect(safeHref('mailto:a@example.com')).toBe('mailto:a@example.com');
    expect(safeHref('//evil.example.com')).toBeNull();
    expect(safeImageSrc('http://x.example.com/a.png')).toBeNull();
    expect(safeImageSrc('https://x.example.com/a.png')).toBe('https://x.example.com/a.png');
    expect(safeImageSrc('/api/v1/components/c1/readme-images/s?u=x')).toBe('/api/v1/components/c1/readme-images/s?u=x');
    expect(safeImageSrc('/other/path.png')).toBeNull();
    expect(safeImageSrc('data:image/png;base64,AAAA')).toBe('data:image/png;base64,AAAA');
    expect(safeImageSrc('data:image/svg+xml,<svg/>')).toBe('data:image/svg+xml,<svg/>');
    expect(safeImageSrc('data:text/html,<script>')).toBeNull();
    expect(safeImageSrc('javascript:alert(1)')).toBeNull();
  });
});
