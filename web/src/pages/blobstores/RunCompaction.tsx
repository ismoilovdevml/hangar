import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { errorText, useCanSystem, useRunTask, useTasks } from '../../api/ops';
import { ConfirmDialog } from '../../components/ConfirmDialog';

/** "Run compaction" for the Reclaimable card: starts the built-in blob compaction task, after a confirmation. */
export function RunCompaction() {
  const canRun = useCanSystem('tasks', 'write');
  const tasks = useTasks();
  const run = useRunTask();
  const [confirm, setConfirm] = useState(false);
  const [started, setStarted] = useState<string | null>(null);
  const task = tasks.data?.items.find((t) => t.type === 'blob_compaction' && t.builtin) ?? tasks.data?.items.find((t) => t.type === 'blob_compaction');
  if (!canRun || !task) return null;
  const running = tasks.data?.running.some((r) => r.task_id === task.id);

  if (started || running) {
    return (
      <Link to="/tasks/$taskId" params={{ taskId: task.id }} search={started ? { run: started } : {}} className="link-strong">
        Compaction running
      </Link>
    );
  }
  return (
    <>
      <button
        type="button"
        className="link-button"
        onClick={() => {
          run.reset();
          setConfirm(true);
        }}
      >
        Run compaction
      </button>
      {confirm && (
        <ConfirmDialog
          title="Run blob compaction now?"
          confirmLabel="Run compaction"
          tone="danger"
          busy={run.isPending}
          error={run.isError ? errorText(run.error, 'The compaction could not be started.') : null}
          onCancel={() => setConfirm(false)}
          onConfirm={() =>
            run.mutate(task.id, {
              onSuccess: (r) => {
                setConfirm(false);
                setStarted(r.id);
              },
            })
          }
        >
          <p>Soft-deleted blobs are removed from the stores for good and their space is freed. It runs outside its schedule ({task.schedule_text}).</p>
        </ConfirmDialog>
      )}
    </>
  );
}
