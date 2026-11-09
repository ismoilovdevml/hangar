import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { MarkdownInline } from '../components/Markdown';
import { findingsState } from '../mocks/findings';
import { advisoryExcerpt } from '../lib/vulnerabilities';
import { AdvisoryText } from '../pages/vulnerabilities/AdvisoryText';
import { renderApp } from './render';

/** OSV `details` as advisories really write them: headings, emphasis, inline code and a long PoC. */
const DETAILS = [
  '### Summary',
  'The `XmlDecryptor` accepts **unsigned** payloads, so an attacker can bypass signature checks.',
  '',
  '### Details',
  'Line one of the details.',
  '',
  '### PoC',
  '```python',
  ...Array.from({ length: 40 }, (_, i) => `print("step ${i}")`),
  '```',
  '',
  '<img src="https://evil.example/pixel.gif"> <script>alert(1)</script>',
  '![diagram](https://evil.example/diagram.png)',
].join('\n');

describe('advisoryExcerpt', () => {
  it('keeps the first section and never a code block', () => {
    expect(advisoryExcerpt(DETAILS)).toBe(
      '### Summary\nThe `XmlDecryptor` accepts **unsigned** payloads, so an attacker can bypass signature checks.',
    );
    expect(advisoryExcerpt('Intro.\n\n```sh\nexploit\n```')).toBe('Intro.');
  });

  it('stops after a few lines of a long first section', () => {
    const long = Array.from({ length: 20 }, (_, i) => `Sentence ${i}.`).join('\n');
    expect(advisoryExcerpt(long, 8)!.split('\n')).toHaveLength(8);
  });

  it('is null when the whole text is short', () => {
    expect(advisoryExcerpt('A short description.')).toBeNull();
    expect(advisoryExcerpt('### Summary\nShort.')).toBeNull();
  });
});

describe('AdvisoryText', () => {
  it('renders headings, emphasis and code, collapsed to the first section', async () => {
    const user = userEvent.setup();
    const { container } = render(<AdvisoryText source={DETAILS} />);
    // The shallowest heading becomes an h4, under the drawer's h3 "Summary".
    expect(screen.getByRole('heading', { level: 4, name: 'Summary' })).toBeInTheDocument();
    expect(screen.getByText('XmlDecryptor', { selector: 'code' })).toBeInTheDocument();
    expect(screen.getByText('unsigned', { selector: 'strong' })).toBeInTheDocument();
    expect(container.textContent).not.toContain('###');
    expect(container.textContent).not.toContain('**');
    expect(screen.queryByText(/step 3/)).not.toBeInTheDocument();

    const toggle = screen.getByRole('button', { name: 'Show full advisory' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(screen.getByRole('button', { name: 'Show less' })).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('heading', { level: 4, name: 'PoC' })).toBeInTheDocument();
    const code = container.querySelector('pre.md-code')!;
    expect(code).toHaveTextContent('print("step 39")');
    // No raw HTML, no script and no image is loaded.
    expect(container.querySelector('script, img')).toBeNull();
    expect(container.textContent).not.toContain('alert(1)');
    expect(screen.getByRole('img', { name: 'diagram' })).toHaveClass('md-img-chip');
  });

  it('shows a short advisory whole, without a toggle', () => {
    render(<AdvisoryText source="Use **1.2.3** or later." />);
    expect(screen.getByText('1.2.3', { selector: 'strong' })).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});

describe('MarkdownInline', () => {
  it('formats code and emphasis but turns links into text', () => {
    const { container } = render(
      <button type="button">
        <MarkdownInline source="`yaml.load` allows code execution, see [the advisory](https://example.com)" />
      </button>,
    );
    expect(screen.getByText('yaml.load', { selector: 'code' })).toBeInTheDocument();
    expect(container.querySelector('a')).toBeNull();
    expect(container).toHaveTextContent('yaml.load allows code execution, see the advisory');
  });
});

describe('finding drawer', () => {
  it('renders the advisory as Markdown, compact until asked', async () => {
    findingsState.findings.find((f) => f.id === 'f_8')!.details = DETAILS;
    const user = userEvent.setup();
    renderApp('/vulnerabilities?finding=f_8');
    const drawer = await screen.findByRole('dialog', { name: '.NET XML encryption security feature bypass' });
    const summary = await within(drawer).findByRole('region', { name: 'Summary' });
    await within(summary).findByText('unsigned', { selector: 'strong' });
    expect(summary.textContent).not.toContain('###');
    await user.click(within(summary).getByRole('button', { name: 'Show full advisory' }));
    expect(within(summary).getByRole('heading', { level: 4, name: 'PoC' })).toBeInTheDocument();
  });
});
