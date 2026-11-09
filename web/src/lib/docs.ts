/** The project's documentation, read on the public repository (the server does not serve it). */
export const DOCS_URL = 'https://github.com/ismoilovdevml/hangar/tree/main/docs';

export const DOCS = {
  guides: `${DOCS_URL}/guides`,
  configuration: `${DOCS_URL}/deploy/README.md#configuration-in-one-paragraph`,
  backups: `${DOCS_URL}/deploy/backup-restore.md`,
  highAvailability: `${DOCS_URL}/deploy/high-availability.md`,
} as const;
