#ifndef EK_H
#define EK_H

// Each call returns memory the caller owns and frees. On failure the result is NULL and *err
// holds a message; otherwise *err is left alone.
char *ek_writable(char **err);
// The calendar new events land on, or an empty string when the store names none.
char *ek_default_calendar(char **err);
char *ek_events(const char *calendar, double from, double to, char **err);
char *ek_create(const char *calendar, const char *title, const char *location, const char *notes,
                double start, double end, int all_day, int alarm_minutes, char **err);
// The event as JSON, or "null" when the uid names nothing on that calendar.
char *ek_find(const char *calendar, const char *uid, char **err);
// 1 removed, 0 nothing matched, -1 failed.
int ek_delete(const char *calendar, const char *uid, char **err);

#endif
