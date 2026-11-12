//go:build darwin

#import <EventKit/EventKit.h>
#import <Foundation/Foundation.h>

#include <stdlib.h>

#include "ek.h"

// The statics below hold objects past the autorelease pool that created them, which is only safe
// because ARC makes a file-static object pointer strong. Dropping -fobjc-arc would leave them
// dangling rather than failing to compile, so the flag is checked instead of assumed.
#if !__has_feature(objc_arc)
#error "internal/calendar must be compiled with -fobjc-arc"
#endif

static char *copyString(NSString *text) { return strdup(text.UTF8String ?: ""); }

static void setError(char **err, NSString *message) {
  if (err) *err = copyString(message);
}

// One store and one access request per process. Building an EKEventStore is what loads the
// calendar database, so a fresh one per call pays that cost every time; the access request is
// asynchronous, so the semaphore turns it back into something a Go call can wait on.
static EKEventStore *sharedStore(char **err) {
  static EKEventStore *store = nil;
  static BOOL granted = NO;
  static NSString *refusal = nil;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    store = [[EKEventStore alloc] init];
    if (@available(macOS 14.0, *)) {
      dispatch_semaphore_t sem = dispatch_semaphore_create(0);
      __block BOOL allowed = NO;
      __block NSString *failure = nil;
      [store requestFullAccessToEventsWithCompletion:^(BOOL ok, NSError *error) {
        allowed = ok;
        failure = error.localizedDescription;
        dispatch_semaphore_signal(sem);
      }];
      dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
      granted = allowed;
      refusal = failure ?: @"calendar access was refused; grant it in System Settings > Privacy & Security > Calendars";
    } else {
      refusal = @"reading the calendar needs macOS 14 or newer";
    }
  });
  if (!granted) {
    setError(err, refusal);
    return nil;
  }
  return store;
}

static EKCalendar *findCalendar(EKEventStore *store, const char *name, char **err) {
  NSString *wanted = @(name);
  for (EKCalendar *cal in [store calendarsForEntityType:EKEntityTypeEvent]) {
    if ([cal.title isEqualToString:wanted]) return cal;
  }
  setError(err, [NSString stringWithFormat:@"no calendar named %@", wanted]);
  return nil;
}

static char *encode(id object, char **err) {
  NSError *jsonError = nil;
  NSData *data = [NSJSONSerialization dataWithJSONObject:object options:0 error:&jsonError];
  if (!data) {
    setError(err, jsonError.localizedDescription ?: @"the calendar result could not be encoded");
    return NULL;
  }
  return copyString([[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding]);
}

char *ek_writable(char **err) {
  @autoreleasepool {
    EKEventStore *store = sharedStore(err);
    if (!store) return NULL;
    NSMutableArray<NSString *> *titles = [NSMutableArray array];
    for (EKCalendar *cal in [store calendarsForEntityType:EKEntityTypeEvent]) {
      if (cal.allowsContentModifications) [titles addObject:cal.title ?: @""];
    }
    return encode(titles, err);
  }
}

char *ek_default_calendar(char **err) {
  @autoreleasepool {
    EKEventStore *store = sharedStore(err);
    if (!store) return NULL;
    EKCalendar *cal = store.defaultCalendarForNewEvents;
    return copyString(cal.allowsContentModifications ? cal.title : @"");
  }
}

char *ek_events(const char *calendar, double from, double to, char **err) {
  @autoreleasepool {
    EKEventStore *store = sharedStore(err);
    if (!store) return NULL;
    EKCalendar *cal = findCalendar(store, calendar, err);
    if (!cal) return NULL;

    NSPredicate *window =
        [store predicateForEventsWithStartDate:[NSDate dateWithTimeIntervalSince1970:from]
                                       endDate:[NSDate dateWithTimeIntervalSince1970:to]
                                     calendars:@[ cal ]];
    NSMutableArray *rows = [NSMutableArray array];
    for (EKEvent *event in [store eventsMatchingPredicate:window]) {
      if (!event.startDate || !event.endDate) continue;
      [rows addObject:@{
        @"uid" : event.eventIdentifier ?: @"",
        @"title" : event.title ?: @"",
        @"start" : @(event.startDate.timeIntervalSince1970),
        @"end" : @(event.endDate.timeIntervalSince1970),
        @"location" : event.location ?: @"",
        @"allDay" : @(event.isAllDay),
      }];
    }
    return encode(rows, err);
  }
}

char *ek_create(const char *calendar, const char *title, const char *location, const char *notes,
                double start, double end, int all_day, int alarm_minutes, char **err) {
  @autoreleasepool {
    EKEventStore *store = sharedStore(err);
    if (!store) return NULL;
    EKCalendar *cal = findCalendar(store, calendar, err);
    if (!cal) return NULL;

    EKEvent *event = [EKEvent eventWithEventStore:store];
    event.calendar = cal;
    event.title = @(title);
    event.startDate = [NSDate dateWithTimeIntervalSince1970:start];
    event.endDate = [NSDate dateWithTimeIntervalSince1970:end];
    event.allDay = all_day ? YES : NO;
    if (location && *location) event.location = @(location);
    if (notes && *notes) event.notes = @(notes);
    if (alarm_minutes > 0) {
      [event addAlarm:[EKAlarm alarmWithRelativeOffset:-60.0 * alarm_minutes]];
    }

    NSError *saveError = nil;
    if (![store saveEvent:event span:EKSpanThisEvent commit:YES error:&saveError]) {
      setError(err, saveError.localizedDescription ?: @"the event could not be saved");
      return NULL;
    }
    return copyString(event.eventIdentifier ?: @"");
  }
}

char *ek_find(const char *calendar, const char *uid, char **err) {
  @autoreleasepool {
    EKEventStore *store = sharedStore(err);
    if (!store) return NULL;
    EKEvent *event = [store eventWithIdentifier:@(uid)];
    if (!event || !event.startDate || !event.endDate ||
        ![event.calendar.title isEqualToString:@(calendar)]) {
      return copyString(@"null");
    }
    return encode(@{
      @"uid" : event.eventIdentifier ?: @"",
      @"title" : event.title ?: @"",
      @"start" : @(event.startDate.timeIntervalSince1970),
      @"end" : @(event.endDate.timeIntervalSince1970),
      @"location" : event.location ?: @"",
      @"allDay" : @(event.isAllDay),
    }, err);
  }
}

int ek_delete(const char *calendar, const char *uid, char **err) {
  @autoreleasepool {
    EKEventStore *store = sharedStore(err);
    if (!store) return -1;
    EKEvent *event = [store eventWithIdentifier:@(uid)];
    // An identifier is looked up across the whole store, so the calendar is checked rather than
    // trusted: removing an event from a calendar the caller did not name is the one mistake here
    // that cannot be undone.
    if (!event || ![event.calendar.title isEqualToString:@(calendar)]) return 0;
    // An identifier belonging to a recurring meeting resolves to the series, whose first
    // occurrence is what EKSpanThisEvent would remove — rarely the one the caller meant, and the
    // removal reaches everyone the calendar is shared with. Nothing created here recurs, so the
    // only way to arrive at one is by uid, and refusing is cheaper than guessing.
    if (event.hasRecurrenceRules) {
      setError(err, @"that uid belongs to a recurring event; deleting a single occurrence is a job for the Calendar app");
      return -1;
    }

    NSError *removeError = nil;
    if (![store removeEvent:event span:EKSpanThisEvent commit:YES error:&removeError]) {
      setError(err, removeError.localizedDescription ?: @"the event could not be removed");
      return -1;
    }
    return 1;
  }
}
