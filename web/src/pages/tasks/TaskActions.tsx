import { useState } from 'react';
import { errorText, useRunTask, useStopTask } from '../../api/ops';
import type { Task, TaskRun } from '../../api/opsTypes';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { IconPlay, IconStop } from '../../components/opsIcons';
import { TASK_TYPES } from '../../lib/tasks';

/** Task types that delete or move data: Run now asks first. */
const DESTRUCTIVE = new Set<Task['type']>(['cleanup', 'blob_compaction', 'docker_gc', 'blob_store_move']);

interface TaskActionsProps {
  task: Pick<Task, 'id' | 'name' | 'type'>;
  /** The run in progress, if any. */
  active?: TaskRun;
  size?: 'sm' | 'md';
  onMessage?: (msg: string) => void;
}

/** Run now / Stop for one task, with confirmation where the task deletes data. */
export function TaskActions({ task, active, size = 'sm', onMessage }: TaskActionsProps) {
  const run = useRunTask();
  const stop = useStopTask();
  const [confirm, setConfirm] = useState<'run' | 'stop' | null>(null);
  const cls = `button button--secondary${size === 'sm' ? ' button--sm' : ''}`;

  const doRun = () =>
    run.mutate(task.id, {
      onSuccess: () => {
        setConfirm(null);
        onMessage?.(`${task.name} started.`);
      },
      onError: (e) => {
        if (!confirm) onMessage?.(errorText(e, `${task.name} could not be started.`));
      },
    });
  const doStop = () =>
    stop.mutate(task.id, {
      onSuccess: () => {
        setConfirm(null);
        onMessage?.(`Stop requested for ${task.name}.`);
      },
    });

  return (
    <>
      {active ? (
        <button type="button" className={cls} onClick={() => setConfirm('stop')} aria-label={`Stop ${task.name}`}>
          <IconStop size={14} /> Stop
        </button>
      ) : (
        <button
          type="button"
          className={cls}
          onClick={() => (DESTRUCTIVE.has(task.type) ? setConfirm('run') : doRun())}
          disabled={run.isPending}
          aria-label={`Run ${task.name} now`}
        >
          <IconPlay size={14} /> {run.isPending ? 'Starting…' : 'Run now'}
        </button>
      )}
      {confirm === 'run' && (
        <ConfirmDialog
          title={`Run ${task.name} now?`}
          confirmLabel="Run now"
          tone="danger"
          busy={run.isPending}
          error={run.isError ? errorText(run.error, 'The task could not be started.') : null}
          onCancel={() => {
            setConfirm(null);
            run.reset();
          }}
          onConfirm={doRun}
        >
          <p>
            {TASK_TYPES[task.type]?.hint ?? 'This task changes stored data'}. It runs outside its schedule, on whichever node picks it up
            first.
          </p>
        </ConfirmDialog>
      )}
      {confirm === 'stop' && (
        <ConfirmDialog
          title={`Stop ${task.name}?`}
          confirmLabel="Stop task"
          tone="danger"
          busy={stop.isPending}
          error={stop.isError ? errorText(stop.error, 'The task could not be stopped.') : null}
          onCancel={() => {
            setConfirm(null);
            stop.reset();
          }}
          onConfirm={doStop}
        >
          <p>The run stops at its next safe point. Work already done is kept; the next scheduled run starts over.</p>
        </ConfirmDialog>
      )}
    </>
  );
}
