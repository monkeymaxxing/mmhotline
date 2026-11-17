package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// coveragePageView is what the on-call page shows the member reading it.
// Every fact comes from the code that acts on it: who is on call and whom
// an SOS wakes from the escalation code, the coming weeks from the
// schedule code.
type coveragePageView struct {
	CSRF, Error, Duration string
	Admin                 bool
	// Rhythm is the line under the heading: when the schedule hands over,
	// and how long each person has to claim an SOS.
	Rhythm, EscalateIfUnclaimedAfter string
	OnCallNow                        onCallNowView
	WakeOrder                        wakeOrderView
	Coming                           comingOnCallView
	Availability                     availabilityView
	Taking                           takingOnCallView
	TeamMute                         TeamMute
	MutedUntil                       string
	Team                             []teamMemberView
	Changes                          []coverageChangeView
	Schedule                         scheduleSettingsView
	settingsFileNoticeView
}

// onCallNowView is the person on call now, or Name "Everyone who is
// available" while the schedule names nobody who is, with until when and
// who comes next (Until), and why it is not the schedule's person (Why).
type onCallNowView struct {
	Initial, Name, Until, Why, Muted string
	You                              bool
}

// wakeOrderView is whom an SOS wakes, in order: each step's new people,
// then the people it skips, and what MMHotline does after the last step.
type wakeOrderView struct {
	Steps                []wakeStepView
	Skipped              []personChip
	After, TriageAgentOn string
}

// wakeStepView is one step of waking people: Lead says how it follows the
// step before, such as "no claim in 5 minutes →".
type wakeStepView struct {
	Lead   string
	People []personChip
}

// personChip is a person in the wake order: "You" for the person reading.
// Note says why a skipped person is skipped.
type personChip struct{ Initial, Name, Note string }

// comingOnCallView is who the schedule names in the coming weeks (weekly
// rotation), handovers (by day or by hour), or shifts (custom schedule).
type comingOnCallView struct {
	Title, Note, Problem string
	Periods              []onCallPeriodView
}

type onCallPeriodView struct {
	Label, Name string
	Now         bool
}

// availabilityView is the "Your availability" panel: Status is the
// person's own state, as in the team list, and WhileUnavailable says what
// MMHotline does while they are unavailable.
type availabilityView struct {
	Unavailable             bool
	Until, WhileUnavailable string
	Revision                int64
	Status                  teamMemberView
}

// takingOnCallView is the panel for taking over on call. A person on call
// now (ViewerIsOnCall) is not offered Take on call; the panel says how
// someone else takes over. Others may take it under today's rules:
// CanTake when they are available, from TakeFromMemberID when someone else
// took it.
type takingOnCallView struct {
	ViewerIsOnCall, CanTake, CanGiveBack bool
	YouAreOnCall, Holder                 string
	TakeFromMemberID, TakeFromName       string
	Revision                             int64
}

// teamMemberView is one person in the team list: Status is "on_call",
// "backup", "available", or "unavailable", and Wakes names the contact
// method types that can wake them at night.
type teamMemberView struct {
	Initial, Name, Role, Status, Until, Wakes string
	You                                       bool
}

type coverageChangeView struct {
	At, DateTime, Text string
}

// scheduleSettingsView is the schedule as the settings file sets it, for
// the folded "Where the schedule is set" and, for custom schedule code,
// "The schedule code".
type scheduleSettingsView struct {
	Kind, TimeZone, Handover, FirstStartDate, Backup string
	Slots                                            []scheduleSlotView
	Code                                             string
	TestCases                                        []customScheduleTestCaseRow
	StoredShifts                                     []customScheduleShiftRow
	Versions                                         []string
}

type scheduleSlotView struct{ Label, Name string }

// customScheduleTestCaseRow is one test case of the saved Custom schedule.
type customScheduleTestCaseRow struct {
	When, ExpectedPrimary, ExpectedBackup string
}

// customScheduleShiftRow is one stored shift of the Custom schedule.
type customScheduleShiftRow struct{ When, Primary, Backup string }

func (a *App) RegisterCoverageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /on-call", a.coveragePage)
	mux.HandleFunc("POST /on-call", a.coverageMutation)
}

func (a *App) coveragePage(w http.ResponseWriter, r *http.Request) {
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		a.browserAccessError(w, r, err)
		return
	}
	a.renderCoverage(w, r, p, 200, "")
}

// renderCoverage shows who is on call and whom an SOS wakes, the coming
// weeks, the people's own availability, taking over on call, team mute,
// and the team. The settings file sets the schedule.
func (a *App) renderCoverage(w http.ResponseWriter, r *http.Request, p Principal, status int, problem string) {
	v := coveragePageView{CSRF: a.CSRFToken(w, r), Error: problem, Duration: "2h", settingsFileNoticeView: a.settingsFileNoticeView()}
	now := a.Now().UTC()
	ctx := r.Context()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return v.read(ctx, tx, p, now)
	})
	if err != nil {
		HTTPError(w, err)
		return
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, "On call", frame, coverageHeading("On call", v), coverageBody(v))
}

// read fills the page for member p at now, in one transaction.
func (v *coveragePageView) read(ctx context.Context, tx *sql.Tx, p Principal, now time.Time) error {
	viewer, err := RequireMember(ctx, tx, p, false)
	if err != nil {
		return err
	}
	v.Admin = viewer.Role == RoleAdministrator
	zone, err := teamTimeZone(ctx, tx)
	if err != nil {
		return err
	}
	clock := func(t time.Time) string { return pageClock(t, now, zone) }
	personal, err := ReadPersonalAvailability(ctx, tx, viewer.ID)
	if err != nil {
		return err
	}
	coverage, err := CoverageAt(ctx, tx, now)
	if err != nil {
		return err
	}
	schedules, err := ReadSchedules(ctx, tx)
	if err != nil {
		return err
	}
	var config ScheduleConfig
	for _, c := range schedules.Configs {
		if c.Kind == schedules.ActiveKind {
			config = c
		}
	}
	window, err := escalateIfUnclaimedAfter(ctx, tx)
	if err != nil {
		return err
	}
	team, err := readOnCallTeam(ctx, tx, now)
	if err != nil {
		return err
	}
	name := func(id string) string {
		switch {
		case id == "":
			return "No one scheduled"
		case team.names[id] == "":
			return "Removed member"
		}
		return team.names[id]
	}
	// you names the person reading "you", and everyone else by name.
	you := func(id string) string {
		if id == viewer.ID {
			return "you"
		}
		return name(id)
	}
	wakes, err := contactMethodTypesThatWakeEachMember(ctx, tx)
	if err != nil {
		return err
	}
	var triageAgentSetUp bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup)`).Scan(&triageAgentSetUp); err != nil {
		return err
	}

	// Whom an SOS wakes, as it is, and as it would be while the person
	// reading is unavailable.
	order, err := sosWakeOrder(now, window, func(at time.Time) (Coverage, error) { return CoverageAt(ctx, tx, at) })
	if err != nil {
		return err
	}
	orderWhileUnavailable, err := sosWakeOrder(now, window, func(at time.Time) (Coverage, error) { return coverageAt(ctx, tx, at, viewer.ID) })
	if err != nil {
		return err
	}

	// The coming weeks, handovers, or shifts, from the schedule code.
	var timeline CustomScheduleTimelineState
	var storedShifts []CustomScheduleShift
	if config.Kind == customScheduleKind {
		if storedShifts, timeline, err = ReadCustomScheduleTimeline(ctx, tx, now); err != nil {
			return err
		}
	}
	instants, err := comingScheduleInstants(ctx, tx, config, now)
	if err != nil {
		return err
	}
	periods, err := comingOnCallPeriods(ctx, tx, config, instants)
	if err != nil {
		return err
	}

	// Until when the person on call now stays on call, and who follows.
	changeAt, next, err := nextOnCallChange(ctx, tx, now, coverage)
	if err != nil {
		return err
	}

	onCall, why := onCallMemberID(coverage), coverage.Reason
	if order[0].Step == "only_member" && len(order[0].Targets) == 1 {
		// The team's only member is woken first, scheduled or not.
		onCall, why = order[0].Targets[0], ""
	}
	v.Rhythm = scheduleRhythm(config, window)
	v.EscalateIfUnclaimedAfter = plural(int(window/time.Minute), "minute")
	v.OnCallNow = onCallNowView{Initial: initial(name(onCall)), Name: name(onCall), You: onCall == viewer.ID, Why: why}
	switch {
	case onCall != "":
	case len(coverage.EligibleMemberIDs) == 0:
		v.OnCallNow = onCallNowView{Name: "Everyone on the team", Why: why}
	default:
		v.OnCallNow = onCallNowView{Name: "Everyone who is available", Why: why}
	}
	if !changeAt.IsZero() {
		v.OnCallNow.Until = "Until " + clock(changeAt) + ", " + relativeTime(changeAt, now) + ". Then " + onCallText(next, you) + "."
	}
	claim := coverage.OnCallClaim
	switch {
	case claim.Active && onCall == claim.MemberID:
		scheduled := "no one now"
		if coverage.AssignedPrimaryID != "" {
			scheduled = you(coverage.AssignedPrimaryID)
		}
		v.OnCallNow.Why = capitalize(you(claim.MemberID)) + " took on call. The schedule names " + scheduled + "."
	case claim.Active && claim.MemberID == viewer.ID:
		v.OnCallNow.Why = "You took on call but are unavailable now, so the schedule is used."
	case claim.Active:
		v.OnCallNow.Why = name(claim.MemberID) + " took on call but is unavailable or no longer on the team, so the schedule is used."
	}
	if coverage.TeamMute.Active {
		v.MutedUntil = clock(coverage.TeamMute.Until)
		v.OnCallNow.Muted = "The team is muted until " + v.MutedUntil + ": MMHotline sends no Pushover notifications or automated voice calls, so an SOS wakes nobody until then."
	}
	v.TeamMute = coverage.TeamMute

	v.WakeOrder = wakeOrderText(order, viewer.ID, window, you, name)
	for _, id := range team.ids {
		if !slices.ContainsFunc(order, func(s sosWakeStep) bool { return containsMember(s.Targets, id) }) {
			chip := personChip{Initial: initial(name(id)), Name: capitalize(you(id))}
			if until, ok := team.unavailableUntil[id]; ok {
				chip.Note = "skipped: unavailable until " + clock(until)
			} else {
				chip.Note = "skipped"
			}
			v.WakeOrder.Skipped = append(v.WakeOrder.Skipped, chip)
		}
	}
	if triageAgentSetUp {
		wait, err := sosWaitsForTheAgent(ctx, tx)
		if err != nil {
			return err
		}
		v.WakeOrder.TriageAgentOn = "When an SOS comes from an alert source, MMHotline first gives the triage agent up to " + durationText(wait) + " to look into it before waking anyone."
		if wait == 0 {
			v.WakeOrder.TriageAgentOn = "When an SOS comes from an alert source, MMHotline wakes the person on call at once, and the triage agent looks into it at the same time."
		}
	}

	v.Coming = comingOnCallText(config, periods, now, zone, you)
	if config.Kind == customScheduleKind {
		v.Coming.Note = "MMHotline works out who is on call from your schedule code every hour, " + strconv.Itoa(customScheduleDaysWorkedOutAhead) + " days ahead."
		if !timeline.ShiftsUntil.After(now) {
			v.Coming.Problem = "MMHotline cannot list the coming shifts: none are worked out from your schedule code."
		}
		if timeline.LastFailureMessage != "" {
			v.Coming.Problem = strings.TrimSpace(v.Coming.Problem + " The last update of the coming shifts failed: " + timeline.LastFailureMessage)
		}
	}

	status := func(id string) teamMemberView {
		m := teamMemberView{Initial: initial(name(id)), Name: name(id), You: id == viewer.ID, Status: "available"}
		until, unavailable := team.unavailableUntil[id]
		switch {
		case id == onCall:
			m.Status = "on_call"
		case len(order) > 1 && order[1].Step == "backup" && containsMember(order[1].MemberIDs, id):
			m.Status = "backup"
		case unavailable:
			m.Status, m.Until = "unavailable", clock(until)
		}
		var titles []string
		for _, kind := range wakes[id] {
			if facts, ok := contactMethodTypeFacts(kind); ok {
				titles = append(titles, cardTitleInSentence(facts))
			}
		}
		m.Wakes = namesInSentence(titles)
		return m
	}
	for _, member := range team.members {
		m := status(member.ID)
		m.Role = "Member"
		if member.Role == RoleAdministrator {
			m.Role = "Administrator"
		}
		v.Team = append(v.Team, m)
	}

	v.Availability = availabilityView{Unavailable: personal.UnavailableUntil.After(now), Revision: personal.Revision, Status: status(viewer.ID)}
	if v.Availability.Unavailable {
		v.Availability.Until = clock(personal.UnavailableUntil)
	}
	v.Availability.WhileUnavailable = whileUnavailableText(viewer.ID, v.Availability.Unavailable, order, orderWhileUnavailable, name)

	v.Taking = takingOnCallView{ViewerIsOnCall: onCall == viewer.ID, Revision: claim.Revision}
	if v.Taking.ViewerIsOnCall {
		v.Taking.YouAreOnCall = "You are on call."
		if !changeAt.IsZero() {
			v.Taking.YouAreOnCall = "You are on call until " + clock(changeAt) + "."
		}
	}
	if claim.Active {
		v.Taking.Holder = capitalize(you(claim.MemberID)) + " took on call until " + clock(claim.Until) + "."
		if claim.MemberID != viewer.ID {
			v.Taking.TakeFromMemberID, v.Taking.TakeFromName = claim.MemberID, name(claim.MemberID)
		}
	}
	// Today's rules: an available member may take on call, from whoever
	// took it with take_from_member_id; whoever took it, or an
	// administrator, gives it back.
	v.Taking.CanTake = !v.Taking.ViewerIsOnCall && containsMember(coverage.EligibleMemberIDs, viewer.ID)
	v.Taking.CanGiveBack = claim.Active && (v.Admin || claim.MemberID == viewer.ID)

	changes, err := readCoverageChanges(ctx, tx)
	if err != nil {
		return err
	}
	for _, change := range changes {
		v.Changes = append(v.Changes, coverageChangeView{At: clock(change.At), DateTime: change.At.UTC().Format(time.RFC3339), Text: coverageChangeText(change, name, clock)})
	}

	v.Schedule = scheduleSettings(config, name, clock, storedShifts)
	if config.Kind == customScheduleKind {
		versions, err := ReadCustomScheduleCodeVersions(ctx, tx)
		if err != nil {
			return err
		}
		for _, version := range versions {
			v.Schedule.Versions = append(v.Schedule.Versions, clock(version.CreatedAt))
		}
	}
	return nil
}

// onCallTeam is the team for the on-call page: the members in name order,
// every name a change may mention (removed members' too), and who is
// unavailable, until when.
type onCallTeam struct {
	members          []Member
	ids              []string
	names            map[string]string
	unavailableUntil map[string]time.Time
}

// readOnCallTeam reads the team at now. A member is unavailable while
// their end time is ahead, as CoverageAt counts it, even before
// ExpireCoverage clears an end that passed.
func readOnCallTeam(ctx context.Context, tx *sql.Tx, now time.Time) (onCallTeam, error) {
	team := onCallTeam{names: map[string]string{}, unavailableUntil: map[string]time.Time{}}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.sign_in_email_address,m.name,m.role,m.removed_at,COALESCE(a.unavailable_until,0) FROM members m LEFT JOIN member_availability a ON a.member_id=m.id ORDER BY m.name,m.id`)
	if err != nil {
		return team, err
	}
	for rows.Next() {
		var m Member
		var removedAt sql.NullInt64
		var unavailable int64
		if err = rows.Scan(&m.ID, &m.SignInEmailAddress, &m.Name, &m.Role, &removedAt, &unavailable); err != nil {
			rows.Close()
			return team, err
		}
		team.names[m.ID] = m.Name
		if removedAt.Valid {
			continue
		}
		team.members = append(team.members, m)
		team.ids = append(team.ids, m.ID)
		if unavailable > now.Unix() {
			team.unavailableUntil[m.ID] = deadlineTime(unavailable)
		}
	}
	return team, errors.Join(rows.Err(), rows.Close())
}

func readCoverageChanges(ctx context.Context, tx *sql.Tx) ([]CoverageChange, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind,actor_member_id,member_id,occurred_at,until_at,title FROM coverage_changes ORDER BY id DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	var changes []CoverageChange
	for rows.Next() {
		var change CoverageChange
		var at, until int64
		if err = rows.Scan(&change.Kind, &change.ActorMemberID, &change.MemberID, &at, &until, &change.Title); err != nil {
			rows.Close()
			return nil, err
		}
		change.At, change.Until = deadlineTime(at), deadlineTime(until)
		changes = append(changes, change)
	}
	return changes, errors.Join(rows.Err(), rows.Close())
}

// sosWakeStep is one step of waking people for an SOS: the step as the
// escalation code names it, everyone it wakes (Targets), the people among
// them no earlier step woke (MemberIDs), and whether nobody was available,
// so it wakes every member (EveryMember).
type sosWakeStep struct {
	Step               string
	Targets, MemberIDs []string
	EveryMember        bool
}

// sosWakeOrder says whom an SOS that arrives at now wakes, step by step,
// by running the escalation code's own choice on a new incident:
// chooseInitialTarget at now, as createIncident does, then chooseNextTarget
// at each step's deadline, as ReconcileIncidents does, with the coverage
// that coverageAt gives for that moment. The rules of escalation stay in
// those two functions. The loop ends because the escalation code's steps
// are a short ladder: only the person on call and the backup have a
// deadline, and the last step (everyone available, or the team's only
// member) has none; MMHotline keeps waking its people until someone claims
// the SOS.
func sosWakeOrder(now time.Time, window time.Duration, coverageAt func(time.Time) (Coverage, error)) ([]sosWakeStep, error) {
	c, err := coverageAt(now)
	if err != nil {
		return nil, err
	}
	var i Incident
	chooseInitialTarget(&i, c, window, now)
	var steps []sosWakeStep
	var woken []string
	for {
		step := sosWakeStep{Step: i.Step, Targets: i.TargetMemberIDs, EveryMember: i.IgnorePersonal}
		for _, id := range i.TargetMemberIDs {
			if !containsMember(woken, id) {
				step.MemberIDs = append(step.MemberIDs, id)
				woken = append(woken, id)
			}
		}
		steps = append(steps, step)
		if i.Deadline.IsZero() {
			return steps, nil
		}
		at := i.Deadline
		if c, err = coverageAt(at); err != nil {
			return nil, err
		}
		chooseNextTarget(&i, c, window, at)
	}
}

// wakeOrderText writes the wake order for the page: each step that wakes
// someone new, and what MMHotline does after the last step. you names a
// member, the person reading as "you".
func wakeOrderText(order []sosWakeStep, viewerID string, window time.Duration, you, name func(string) string) wakeOrderView {
	var view wakeOrderView
	wait := "no claim in " + plural(int(window/time.Minute), "minute") + " →"
	for n, step := range order {
		if len(step.MemberIDs) == 0 {
			continue
		}
		var lead string
		switch {
		case step.EveryMember:
			lead = "Nobody is available, so everyone on the team:"
		case step.Step == "everyone_available" && n == 0:
			lead = "Everyone who is available:"
		case step.Step == "everyone_available":
			lead = wait + " everyone else who is available:"
		case n > 0:
			lead = wait
		}
		// The person reading comes first, as "You".
		ids := step.MemberIDs
		if at := slices.Index(ids, viewerID); at > 0 {
			ids = append([]string{viewerID}, slices.Delete(slices.Clone(ids), at, at+1)...)
		}
		var people []personChip
		for _, id := range ids {
			people = append(people, personChip{Initial: initial(name(id)), Name: capitalize(you(id))})
		}
		view.Steps = append(view.Steps, wakeStepView{Lead: lead, People: people})
	}
	last := order[len(order)-1]
	var whom []string
	for _, id := range last.Targets {
		whom = append(whom, you(id))
	}
	switch {
	case len(last.Targets) == 0:
		view.After = "The team has no members yet."
	case last.Step == "only_member" && last.Targets[0] == viewerID:
		view.After = "MMHotline keeps waking you until you claim the incident."
	case last.EveryMember:
		view.After = "MMHotline keeps waking everyone on the team until someone claims the incident."
	case last.Step == "everyone_available" && len(last.Targets) > 1:
		view.After = "MMHotline keeps waking everyone who is available until someone claims the incident."
	default:
		view.After = "MMHotline keeps waking " + namesInSentence(whom) + " until someone claims the incident."
	}
	return view
}

// whileUnavailableText says what MMHotline does while the person reading
// is unavailable, from the wake order as it is (order) and as it is with
// them unavailable (orderWhileUnavailable).
func whileUnavailableText(viewerID string, unavailable bool, order, orderWhileUnavailable []sosWakeStep, name func(string) string) string {
	first := orderWhileUnavailable[0]
	switch {
	case first.EveryMember && unavailable:
		return "Nobody else is available either, so an SOS still wakes you: when nobody is available, MMHotline wakes everyone on the team."
	case first.EveryMember:
		return "Nobody else is available, so an SOS would still wake you: when nobody is available, MMHotline wakes everyone on the team."
	case unavailable:
		return "Until then, MMHotline skips you when it wakes people for an SOS."
	case !containsMember(order[0].MemberIDs, viewerID):
		return "While you are unavailable, MMHotline skips you when it wakes people for an SOS."
	case first.Step == "everyone_available":
		return "While you are unavailable, MMHotline skips you and wakes everyone else who is available first."
	}
	var names []string
	for _, id := range first.MemberIDs {
		names = append(names, name(id))
	}
	return "While you are unavailable, MMHotline skips you and wakes " + namesInSentence(names) + " first."
}

// onCallText names who is on call under coverage c in a sentence.
func onCallText(c Coverage, you func(string) string) string {
	switch id := onCallMemberID(c); {
	case id != "":
		return you(id)
	case len(c.EligibleMemberIDs) == 0:
		return "everyone on the team, because nobody is available"
	}
	return "everyone who is available"
}

// onCallPeriod is a stretch of time in which the schedule names one
// person, MemberID, or nobody (""). End is zero when the stretch goes on
// past the ones listed.
type onCallPeriod struct {
	Start, End time.Time
	MemberID   string
}

// comingOnCallPeriods asks the schedule code whom it names at each of
// instants, the first being now, and joins neighbours that name the same
// person. Each period ends at the next instant that names someone else;
// the last one goes on past the instants.
func comingOnCallPeriods(ctx context.Context, tx *sql.Tx, c ScheduleConfig, instants []time.Time) ([]onCallPeriod, error) {
	var periods []onCallPeriod
	for _, at := range instants {
		assignment, err := scheduledAssignment(ctx, tx, c, at)
		if err != nil {
			return nil, err
		}
		//nolint:nilaway // n is len(periods) and is above 0.
		if n := len(periods); n > 0 {
			if periods[n-1].MemberID == assignment.PrimaryID {
				continue
			}
			periods[n-1].End = at
		}
		periods = append(periods, onCallPeriod{Start: at, MemberID: assignment.PrimaryID})
	}
	return periods, nil
}

// comingOnCallText labels the coming periods for the page: a weekly
// rotation by its weeks, until every person in the rotation has a coming
// week (at least four), the other schedules by their handovers or shifts.
func comingOnCallText(c ScheduleConfig, periods []onCallPeriod, now time.Time, zone *time.Location, you func(string) string) comingOnCallView {
	view := comingOnCallView{Note: "An administrator changes the schedule in MMHotline's settings file."}
	when := func(t time.Time) string { return pageClock(t, now, zone) }
	switch c.Kind {
	case "":
		return comingOnCallView{}
	case "weekly_rotation":
		view.Title = "Week by week"
		periods = periods[:min(len(periods), max(4, len(c.RotationMemberIDs)+1))]
		when = func(t time.Time) string { return pageDay(t, now, zone) }
	case "day_of_week":
		view.Title = "Coming handovers"
		when = func(t time.Time) string { return pageDay(t, now, zone) }
	case "hour_of_day":
		view.Title = "Coming handovers"
	case customScheduleKind:
		view.Title = "Coming shifts"
	}
	for n, period := range periods {
		p := onCallPeriodView{Name: capitalize(you(period.MemberID)), Now: n == 0}
		switch {
		case n == 0 && c.Kind == "weekly_rotation":
			p.Label = "This week"
		case n == 0:
			p.Label = "Now"
		case period.End.IsZero():
			p.Label = "From " + when(period.Start)
		default:
			p.Label = when(period.Start) + " – " + when(period.End)
		}
		if n == 0 && !period.End.IsZero() {
			p.Label += " · until " + when(period.End)
		}
		view.Periods = append(view.Periods, p)
	}
	return view
}

// pageDay writes the day of a time as pages show it, without the time of
// day: "3 Nov", or "3 Nov 2027" in another year.
func pageDay(t, now time.Time, zone *time.Location) string {
	if t.In(zone).Year() != now.In(zone).Year() {
		return t.In(zone).Format("2 Jan 2006")
	}
	return t.In(zone).Format("2 Jan")
}

// scheduleRhythm is the line under the page's heading: when the schedule
// hands over, and how long each person has to claim an SOS before
// MMHotline wakes the next one.
func scheduleRhythm(c ScheduleConfig, window time.Duration) string {
	handover := fmt.Sprintf("%02d:%02d", c.HandoffHour, c.HandoffMinute)
	rhythm := "No schedule yet"
	switch c.Kind {
	case "weekly_rotation":
		rhythm = "Handover every " + time.Weekday(c.HandoffWeekday%7).String() + " at " + handover
	case "day_of_week":
		rhythm = "Handover every day at " + handover
	case "hour_of_day":
		rhythm = "A person for each hour of the day"
	case customScheduleKind:
		rhythm = "Your schedule code says who is on call"
	}
	return rhythm + " · " + plural(int(window/time.Minute), "minute") + " to claim an SOS"
}

// coverageChangeText says what a recorded change did, with its time as the
// page writes times (clock). The titles kept in the history are alert
// titles, with their own time format.
func coverageChangeText(c CoverageChange, name func(string) string, clock func(time.Time) string) string {
	switch c.Kind {
	case "personal_unavailable":
		if c.Until.IsZero() {
			return name(c.MemberID) + " is available again."
		}
		return name(c.MemberID) + " is unavailable until " + clock(c.Until) + "."
	case "personal_unavailable_expired":
		return name(c.MemberID) + " is available again: their time as unavailable ended."
	case "team_mute_started":
		return name(c.ActorMemberID) + " muted the team until " + clock(c.Until) + "."
	case "team_mute_ended":
		return name(c.ActorMemberID) + " unmuted the team."
	case "team_mute_expired":
		return "The team mute ended."
	case "on_call_claim_started":
		return name(c.MemberID) + " took on call until " + clock(c.Until) + "."
	case "on_call_claim_ended":
		return name(c.ActorMemberID) + " gave on call back to the schedule."
	case "on_call_claim_expired":
		return "On call is back with the schedule: the time " + name(c.MemberID) + " took it for ended."
	case "schedule_saved":
		return "The schedule changed."
	case "schedule_handoff":
		if c.MemberID == "" {
			return "The schedule names no one now."
		}
		return name(c.MemberID) + " is on call now, by the schedule."
	}
	return c.Title
}

// scheduleSettings is the active schedule as the settings file sets it.
func scheduleSettings(c ScheduleConfig, name func(string) string, clock func(time.Time) string, shifts []CustomScheduleShift) scheduleSettingsView {
	view := scheduleSettingsView{Kind: c.Kind, TimeZone: c.TimeZone, Backup: backupKindLabel(c.BackupKind), Code: c.CustomScheduleCode}
	if c.BackupKind == "fixed_person" {
		view.Backup += ": " + name(c.BackupMemberID)
	}
	handover := fmt.Sprintf("%02d:%02d", c.HandoffHour, c.HandoffMinute)
	switch c.Kind {
	case "weekly_rotation":
		view.Handover = time.Weekday(c.HandoffWeekday%7).String() + " " + handover
		view.FirstStartDate = c.FirstStartDate
		var names []string
		for _, id := range c.RotationMemberIDs {
			names = append(names, name(id))
		}
		view.Slots = []scheduleSlotView{{"Rotation order", strings.Join(names, ", then ")}}
	case "day_of_week":
		view.Handover = "Every day " + handover
		for day, id := range c.Assignments {
			view.Slots = append(view.Slots, scheduleSlotView{time.Weekday(day).String(), name(id)})
		}
	case "hour_of_day":
		// Hours next to each other with the same person show as one.
		for start := 0; start < len(c.Assignments); {
			end := start + 1
			for end < len(c.Assignments) && c.Assignments[end] == c.Assignments[start] {
				end++
			}
			view.Slots = append(view.Slots, scheduleSlotView{fmt.Sprintf("%02d:00–%02d:00", start, end%24), name(c.Assignments[start])})
			start = end
		}
	case customScheduleKind:
		for _, test := range c.CustomScheduleTestCases {
			expectedBackup := "Not checked"
			if test.ExpectedBackupMemberID != "" {
				expectedBackup = name(test.ExpectedBackupMemberID)
			}
			view.TestCases = append(view.TestCases, customScheduleTestCaseRow{When: test.When, ExpectedPrimary: name(test.ExpectedPrimaryMemberID), ExpectedBackup: expectedBackup})
		}
		for _, shift := range shifts {
			backup := backupKindLabel(c.BackupKind)
			switch {
			case shift.BackupMemberID != "":
				backup = name(shift.BackupMemberID)
			case c.BackupKind == "fixed_person":
				backup = name(c.BackupMemberID)
			}
			view.StoredShifts = append(view.StoredShifts, customScheduleShiftRow{When: clock(shift.StartsAt) + " – " + clock(shift.EndsAt), Primary: name(shift.PrimaryMemberID), Backup: backup})
		}
	}
	return view
}

// initial is the first letter of a name, for the person's round badge.
func initial(name string) string {
	r, _ := utf8.DecodeRuneInString(name)
	if r == utf8.RuneError {
		return ""
	}
	return strings.ToUpper(string(r))
}

// coverageMutation saves what people change on the page: their own
// availability, team mute, and taking on call.
func (a *App) coverageMutation(w http.ResponseWriter, r *http.Request) {
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		HTTPError(w, err)
		return
	}
	revision, err := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revision < 0 {
		HTTPError(w, ErrInvalid)
		return
	}
	switch action := r.PostForm.Get("action"); action {
	case "unavailable", "team_mute", "claim_on_call":
		var until time.Time
		if r.PostForm.Get("end") != "1" {
			var duration time.Duration
			duration, err = ParseAvailabilityDuration(r.PostForm.Get("duration"))
			until = a.Now().Add(duration)
		}
		if err == nil {
			switch action {
			case "unavailable":
				_, err = a.SetPersonalAvailability(r.Context(), p, action, until, revision)
			case "team_mute":
				_, err = a.SetTeamMute(r.Context(), p, until, revision)
			case "claim_on_call":
				_, err = a.SetOnCallClaim(r.Context(), p, until, revision, r.PostForm.Get("take_from_member_id"))
			}
		}
	case "end_on_call_claim":
		_, err = a.EndOnCallClaim(r.Context(), p, revision)
	default:
		err = ErrInvalid
	}
	if err != nil {
		message := publicError(err)
		// Every bare conflict here is a revision that changed after the
		// page was loaded, or a take-over of someone no longer on call.
		if message == ErrConflict.Error() {
			message = "The On call page changed while you had it open. Check the page, then choose again."
		}
		a.renderCoverage(w, r, p, errorStatus(err), message)
		return
	}
	http.Redirect(w, r, "/on-call", http.StatusSeeOther)
}
