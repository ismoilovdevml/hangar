/**
 * Editing a task (schedule and on/off) and deleting one an administrator
 * added. Built-in tasks can be edited and disabled, not deleted; the task of
 * a cleanup policy is changed through its policy.
 */
import { useState, type FormEvent } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { errorText, useDeleteTask, useUpdateTask } from '../../api/ops';
import type { Task } from '../../api/opsTypes';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { cronToSchedule, scheduleToCron, type ScheduleDraft } from '../../lib/cron';
import { taskDeletable, taskPolicy } from '../../lib/tasks';
import { ScheduleField } from '../cleanup/ScheduleField';

/** Header actions next to Run now: Edit (or Edit policy) and Delete. */
export function TaskManageActions({ task, editing, onEdit }: { task: Task; editing: boolean; onEdit: () => void }) {
  const navigate = useNavigate();
  const remove = useDeleteTask(task.id);
  const [confirm, setConfirm] = useState(false);
  const policy = taskPolicy(task);

  return (
    <>
      {policy !== null ? (
        <Link to="/cleanup" search={{ policy }} className="button button--secondary">
          Edit policy
        </Link>
      ) : (
        <button type="button" className="button button--secondary" onClick={onEdit} disabled={editing} aria-expanded={editing}>
          Edit
        </button>
      )}
      {taskDeletable(task) && (
        <button
          type="button"
          className="button button--danger-outline"
          onClick={() => {
            remove.reset();
            setConfirm(true);
          }}
        >
          Delete
        </button>
      )}
      {confirm && (
        <ConfirmDialog
          title={`Delete ${task.name}?`}
          confirmLabel="Delete task"
          tone="danger"
          typeToConfirm={task.name}
          busy={remove.isPending}
          error={remove.isError ? errorText(remove.error, 'The task could not be deleted.') : null}
          onCancel={() => setConfirm(false)}
          onConfirm={() => remove.mutate(undefined, { onSuccess: () => void navigate({ to: '/tasks' }) })}
        >
          <p>The task, its schedule and its run history are removed. This cannot be undone.</p>
        </ConfirmDialog>
      )}
    </>
  );
}

/** Schedule and on/off of one task; every other field is sent back unchanged. */
export function TaskEditForm({ task, onSaved, onCancel }: { task: Task; onSaved: () => void; onCancel: () => void }) {
  const [schedule, setSchedule] = useState<ScheduleDraft>(() => cronToSchedule(task.schedule));
  const [enabled, setEnabled] = useState(task.enabled !== false);
  const update = useUpdateTask(task.id);
  const cron = scheduleToCron(schedule);
  const invalid = cron === undefined;
  const changed = cron !== (task.schedule ?? null) || enabled !== (task.enabled !== false);

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (invalid || !changed || update.isPending) return;
    // PUT replaces the task: config and description go back as they are.
    update.mutate(
      { name: task.name, type: task.type, description: task.description, config: task.config, schedule: cron, enabled },
      { onSuccess: onSaved },
    );
  };

  return (
    <form className="task-edit" onSubmit={onSubmit} noValidate aria-label={`Edit ${task.name}`}>
      <ScheduleField
        value={schedule}
        onChange={(v) => {
          update.reset();
          setSchedule(v);
        }}
      />
      <label className="checkbox">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => {
            update.reset();
            setEnabled(e.target.checked);
          }}
        />{' '}
        Enabled <span className="meta">— a disabled task keeps its schedule but does not run until enabled again; Run now still works.</span>
      </label>
      {update.isError && (
        <p className="form-error" role="alert">
          {errorText(update.error, 'The task could not be saved.')}
        </p>
      )}
      <div className="task-edit__actions">
        <button type="submit" className="button button--primary" disabled={invalid || !changed || update.isPending}>
          {update.isPending ? 'Saving…' : 'Save'}
        </button>
        <button type="button" className="button button--secondary" onClick={onCancel} disabled={update.isPending}>
          Cancel
        </button>
      </div>
    </form>
  );
}
