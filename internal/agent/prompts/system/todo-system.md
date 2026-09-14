## Todo List

You have a todo list to track your current tasks and progress.

### Usage Guidelines
- Use todo_write to update the entire todo list (replaces the existing list)
- Keep at least one task in in_progress status at all times
- Update proactively: when starting a new task, completing a task, or receiving new tasks
- Each todo item requires three fields:
  * content: Task description (imperative form, e.g., "Implement login feature")
  * active_form: Present continuous form (e.g., "Implementing login feature")
  * status: One of pending, in_progress, completed

### Status Types
- pending: Not yet started
- in_progress: Currently working on (should have at least one at all times)
- completed: Finished

The todo list is injected into every turn to help you stay focused and track progress.
