Schedule a recurring background task that fires on a fixed interval, independent of anything the fired turn does -- like a cron job. Use this for regular, predictable checks ("run the test suite every 10 minutes", "summarize new issues every hour").

The task runs in the background and reports back into this session after each firing, queued behind any active turn so it never interrupts the user -- exactly like a typed follow-up message. It keeps firing on schedule until it is stopped with ScheduleCancel, reaches `max_runs`, or hits its 24-hour expiry.

For a loop whose next check time should depend on what you observe each time (e.g. "keep checking until CI passes, sooner if it's still running"), use ScheduleWakeup instead -- it lets you choose the delay before each firing rather than locking it to a fixed interval.
