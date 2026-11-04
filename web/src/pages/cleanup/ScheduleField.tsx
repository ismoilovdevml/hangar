import { useId } from 'react';
import { DAY_NAMES, describeCron, nextRuns, parseCron, scheduleToCron, type ScheduleDraft, type ScheduleMode } from '../../lib/cron';
import { formatUtc } from '../../lib/tasks';
import { useNow } from '../../lib/useNow';

const MODES: { id: ScheduleMode; label: string }[] = [
  { id: 'manual', label: 'Manual only' },
  { id: 'hourly', label: 'Every hour' },
  { id: 'daily', label: 'Every day' },
  { id: 'weekly', label: 'Every week' },
  { id: 'custom', label: 'Custom (cron)' },
];

interface ScheduleFieldProps {
  value: ScheduleDraft;
  onChange: (next: ScheduleDraft) => void;
  error?: string;
  disabled?: boolean;
}

/** Schedule builder: common presets, or a raw 5-field cron, always shown in plain words. */
export function ScheduleField({ value, onChange, error, disabled }: ScheduleFieldProps) {
  const id = useId();
  const now = useNow(60_000);
  const cron = scheduleToCron(value);
  const human = cron ? describeCron(cron) : null;
  const next = cron ? nextRuns(cron, now, 1)[0] : undefined;
  const set = (patch: Partial<ScheduleDraft>) => onChange({ ...value, ...patch });
  const parsed = value.mode === 'custom' ? parseCron(value.cron) : null;
  const problem = parsed && !parsed.ok ? parsed.error : cron === undefined ? (error ?? 'Complete the schedule') : undefined;

  return (
    <fieldset className="schedule-field" disabled={disabled}>
      <legend className="field__label">Schedule</legend>
      <div className="schedule-field__row">
        <label className="visually-hidden" htmlFor={`${id}-mode`}>
          How often
        </label>
        <select
          id={`${id}-mode`}
          className="input schedule-field__mode"
          value={value.mode}
          onChange={(e) => {
            const mode = e.target.value as ScheduleMode;
            // Switching to custom starts from the cron the preset stood for.
            set(mode === 'custom' && cron ? { mode, cron } : { mode });
          }}
        >
          {MODES.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label}
            </option>
          ))}
        </select>

        {value.mode === 'hourly' && (
          <label className="schedule-field__inline">
            at minute
            <input
              className="input input--inline"
              type="number"
              inputMode="numeric"
              min={0}
              max={59}
              value={value.minute}
              onChange={(e) => set({ minute: e.target.value === '' ? NaN : Number(e.target.value) })}
            />
          </label>
        )}

        {value.mode === 'weekly' && (
          <label className="schedule-field__inline">
            on
            <select className="input" value={value.weekday} onChange={(e) => set({ weekday: Number(e.target.value) })}>
              {DAY_NAMES.map((d, i) => (
                <option key={d} value={i}>
                  {d}
                </option>
              ))}
            </select>
          </label>
        )}

        {(value.mode === 'daily' || value.mode === 'weekly') && (
          <label className="schedule-field__inline">
            at
            <input className="input input--time" type="time" step={60} value={value.time} onChange={(e) => set({ time: e.target.value })} />
            <span className="meta">UTC</span>
          </label>
        )}

        {value.mode === 'custom' && (
          <label className="schedule-field__inline schedule-field__cron">
            <span className="visually-hidden">Cron expression</span>
            <input
              className="input input--mono"
              value={value.cron}
              onChange={(e) => set({ cron: e.target.value })}
              placeholder="0 3 * * *"
              spellCheck={false}
              autoComplete="off"
              aria-invalid={!!problem || undefined}
              aria-describedby={`${id}-help`}
            />
          </label>
        )}
      </div>
      <p className="field__hint" id={`${id}-help`} aria-live="polite">
        {problem ? (
          <span className="field__error">{problem}</span>
        ) : cron === null ? (
          'Runs only when someone presses Run now.'
        ) : (
          <>
            {human} · <code className="inline-code">{cron}</code>
            {next && <> · next {formatUtc(next.toISOString())}</>}
          </>
        )}
      </p>
    </fieldset>
  );
}
