import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { FormatBadge } from '../components/FormatBadge';
import { FormatIcon, FormatName } from '../components/FormatIcon';

describe('FormatIcon', () => {
  it('draws a named brand mark in the brand colour', () => {
    render(<FormatIcon format="nuget" />);
    const icon = screen.getByRole('img', { name: 'NuGet' });
    expect(icon.querySelector('path')).toHaveAttribute('fill', '#004880');
  });

  it('can follow the text colour and take a custom name', () => {
    render(<FormatIcon format="docker" tone="current" label="Docker images" />);
    expect(screen.getByRole('img', { name: 'Docker images' }).querySelector('path')).toHaveAttribute('fill', 'currentColor');
  });

  it('falls back to a neutral icon for formats without a mark', () => {
    const { container } = render(<FormatIcon format="rpm" />);
    const icon = screen.getByRole('img', { name: 'rpm' });
    expect(icon).toHaveClass('format-icon--generic');
    expect(container.querySelectorAll('path').length).toBeGreaterThan(0);
  });

  it('is hidden from screen readers when a visible name sits next to it', () => {
    render(<FormatName format="pypi" />);
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
    expect(screen.getByText('PyPI')).toBeInTheDocument();
  });

  it('keeps the badge named for screen readers', () => {
    render(<FormatBadge format="maven" />);
    expect(screen.getByTitle('Maven')).toHaveTextContent('Maven');
  });
});
