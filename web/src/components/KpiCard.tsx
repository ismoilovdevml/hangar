import type { ReactNode } from 'react';
import type { Tone } from '../lib/format';
import { Meter, type MeterProps } from './Meter';
import { Pill } from './Pill';

interface KpiCardProps {
  label: string;
  tag?: { text: string; tone: Tone };
  value: ReactNode;
  unit?: ReactNode;
  meter?: MeterProps;
  note?: ReactNode;
}

export function KpiCard({ label, tag, value, unit, meter, note }: KpiCardProps) {
  return (
    <article className="kpi glass" aria-label={label}>
      <div className="kpi__head">
        <h3 className="kpi__label">{label}</h3>
        {tag && <Pill tone={tag.tone}>{tag.text}</Pill>}
      </div>
      <p className="kpi__value">
        <span className="kpi__number">{value}</span>
        {unit && <span className="kpi__unit">{unit}</span>}
      </p>
      {meter && <Meter {...meter} />}
      {note && <p className="kpi__note">{note}</p>}
    </article>
  );
}

export function KpiCardSkeleton({ label }: { label: string }) {
  return (
    <article className="kpi glass" aria-label={label} aria-busy="true">
      <div className="kpi__head">
        <h3 className="kpi__label">{label}</h3>
      </div>
      <p className="kpi__value">
        <span className="skeleton" style={{ width: 110, height: 34 }} />
      </p>
      <Meter value={0} label={label} />
      <p className="kpi__note">
        <span className="skeleton" style={{ width: 160, height: 14 }} />
      </p>
    </article>
  );
}
