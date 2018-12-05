package operator

import (
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robfig/cron"
	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	cbTypes "github.com/operator-framework/operator-metering/pkg/apis/metering/v1alpha1"
	cbutil "github.com/operator-framework/operator-metering/pkg/apis/metering/v1alpha1/util"
	"github.com/operator-framework/operator-metering/pkg/operator/reporting"
	"github.com/operator-framework/operator-metering/pkg/operator/reportingutil"
	"github.com/operator-framework/operator-metering/pkg/util/slice"
)

const (
	scheduledReportFinalizer = cbTypes.GroupName + "/scheduledreport"
)

var (
	scheduledReportPrometheusMetricLabels = []string{"scheduledreport", "reportgenerationquery", "table_name"}

	generateScheduledReportTotalCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusMetricNamespace,
			Name:      "generate_scheduledreports_total",
			Help:      "Duration to generate a Report.",
		},
		scheduledReportPrometheusMetricLabels,
	)

	generateScheduledReportFailedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusMetricNamespace,
			Name:      "generate_scheduledreports_failed_total",
			Help:      "Duration to generate a Report.",
		},
		scheduledReportPrometheusMetricLabels,
	)

	generateScheduledReportDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: prometheusMetricNamespace,
			Name:      "generate_scheduledreport_duration_seconds",
			Help:      "Duration to generate a ScheduledReport.",
			Buckets:   []float64{60.0, 300.0, 600.0},
		},
		scheduledReportPrometheusMetricLabels,
	)
)

func init() {
	prometheus.MustRegister(generateScheduledReportFailedCounter)
	prometheus.MustRegister(generateScheduledReportTotalCounter)
	prometheus.MustRegister(generateScheduledReportDurationHistogram)
}

func (op *Reporting) runScheduledReportWorker() {
	logger := op.logger.WithField("component", "scheduledReportWorker")
	logger.Infof("ScheduledReport worker started")
	const maxRequeues = 5
	for op.processResource(logger, op.syncScheduledReport, "ScheduledReport", op.scheduledReportQueue, maxRequeues) {
	}
}

func (op *Reporting) syncScheduledReport(logger log.FieldLogger, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.WithError(err).Errorf("invalid resource key :%s", key)
		return nil
	}

	logger = logger.WithField("ScheduledReport", name)
	scheduledReport, err := op.scheduledReportLister.ScheduledReports(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Infof("ScheduledReport %s does not exist anymore, stopping and removing any running jobs for ScheduledReport", name)
			return nil
		}
		return err
	}
	sr := scheduledReport.DeepCopy()

	if scheduledReport.DeletionTimestamp != nil {
		_, err = op.removeScheduledReportFinalizer(sr)
		return err
	}

	return op.handleScheduledReport(logger, sr)
}

type reportSchedule interface {
	// Return the next activation time, later than the given time.
	// Next is invoked initially, and then each time the job runs..
	Next(time.Time) time.Time
}

func getSchedule(reportSched *cbTypes.ScheduledReportSchedule) (reportSchedule, error) {
	var cronSpec string
	switch reportSched.Period {
	case cbTypes.ScheduledReportPeriodCron:
		if reportSched.Cron == nil || reportSched.Cron.Expression == "" {
			return nil, fmt.Errorf("spec.schedule.cron.expression must be specified!")
		}
		return cron.ParseStandard(reportSched.Cron.Expression)
	case cbTypes.ScheduledReportPeriodHourly:
		sched := reportSched.Hourly
		if sched == nil {
			sched = &cbTypes.ScheduledReportScheduleHourly{}
		}
		if err := validateMinute(sched.Minute); err != nil {
			return nil, err
		}
		if err := validateSecond(sched.Second); err != nil {
			return nil, err
		}
		cronSpec = fmt.Sprintf("%d %d * * * *", sched.Second, sched.Minute)
	case cbTypes.ScheduledReportPeriodDaily:
		sched := reportSched.Daily
		if sched == nil {
			sched = &cbTypes.ScheduledReportScheduleDaily{}
		}
		if err := validateHour(sched.Hour); err != nil {
			return nil, err
		}
		if err := validateMinute(sched.Minute); err != nil {
			return nil, err
		}
		if err := validateSecond(sched.Second); err != nil {
			return nil, err
		}
		cronSpec = fmt.Sprintf("%d %d %d * * *", sched.Second, sched.Minute, sched.Hour)
	case cbTypes.ScheduledReportPeriodWeekly:
		sched := reportSched.Weekly
		if sched == nil {
			sched = &cbTypes.ScheduledReportScheduleWeekly{}
		}
		dow := 0
		if sched.DayOfWeek != nil {
			var err error
			dow, err = convertDayOfWeek(*sched.DayOfWeek)
			if err != nil {
				return nil, err
			}
		}
		if err := validateHour(sched.Hour); err != nil {
			return nil, err
		}
		if err := validateMinute(sched.Minute); err != nil {
			return nil, err
		}
		if err := validateSecond(sched.Second); err != nil {
			return nil, err
		}
		cronSpec = fmt.Sprintf("%d %d %d * * %d", sched.Second, sched.Minute, sched.Hour, dow)
	case cbTypes.ScheduledReportPeriodMonthly:
		sched := reportSched.Monthly
		if sched == nil {
			sched = &cbTypes.ScheduledReportScheduleMonthly{}
		}
		dom := int64(1)
		if sched.DayOfMonth != nil {
			dom = *sched.DayOfMonth
		}
		if err := validateDayOfMonth(dom); err != nil {
			return nil, err
		}
		if err := validateHour(sched.Hour); err != nil {
			return nil, err
		}
		if err := validateMinute(sched.Minute); err != nil {
			return nil, err
		}
		if err := validateSecond(sched.Second); err != nil {
			return nil, err
		}
		cronSpec = fmt.Sprintf("%d %d %d %d * *", sched.Second, sched.Minute, sched.Hour, dom)
	default:
		return nil, fmt.Errorf("invalid ScheduledReport.spec.schedule.period: %s", reportSched.Period)
	}
	return cron.Parse(cronSpec)
}

func (op *Reporting) handleScheduledReport(logger log.FieldLogger, scheduledReport *cbTypes.ScheduledReport) error {
	if op.cfg.EnableFinalizers && scheduledReportNeedsFinalizer(scheduledReport) {
		var err error
		scheduledReport, err = op.addScheduledReportFinalizer(scheduledReport)
		if err != nil {
			return err
		}
	}

	return op.runScheduledReport(logger, scheduledReport)
}

type reportPeriod struct {
	periodEnd   time.Time
	periodStart time.Time
}

// runScheduledReport takes a scheduledReport, and generates reporting data
// according the report's schedule. If the next scheduled reporting period
// hasn't elapsed, runScheduledReport will requeue the resource for a time when
// the period has elapsed.
func (op *Reporting) runScheduledReport(logger log.FieldLogger, report *cbTypes.ScheduledReport) error {
	// check if this report was previously finished
	runningCond := cbutil.GetScheduledReportCondition(report.Status, cbTypes.ScheduledReportRunning)
	previouslyFinished := runningCond != nil && runningCond.Reason == cbutil.ReportPeriodFinishedReason && runningCond.Status == v1.ConditionFalse

	// if the report's reportingEnd is unset or after the lastReportTime
	// then the report was updated since it last finished and we should
	// consider it something to be reprocessed
	if previouslyFinished {
		if report.Spec.ReportingEnd == nil {
			logger.Infof("previously finished report's spec.reportingEnd is unset: beginning processing of report")
		} else if report.Status.LastReportTime != nil && report.Spec.ReportingEnd.Time.After(report.Status.LastReportTime.Time) {
			logger.Infof("previously finished report's spec.reportingEnd (%s) is now after lastReportTime (%s): beginning processing of report", report.Spec.ReportingEnd.Time, report.Status.LastReportTime.Time)
		} else {
			// return without processing because the report is complete
			logger.Infof("ScheduledReport %s is already finished: %s", report.Name, runningCond.Message)
			return nil
		}
	}

	// validate the scheduledReport before anything else to surface issues
	// before we actually run
	runningCond = cbutil.NewScheduledReportCondition(cbTypes.ScheduledReportRunning, v1.ConditionTrue, cbutil.ValidatingScheduledReportReason, "validating report and its dependencies")
	cbutil.SetScheduledReportCondition(&report.Status, *runningCond)
	var err error
	report, err = op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
	if err != nil {
		logger.WithError(err).Errorf("unable to update ScheduledReport status")
		return err
	}

	if report.Spec.GenerationQueryName == "" {
		return op.setScheduledReportStatusValidationFailure(report, "must set spec.generationQuery")
	}

	if report.Spec.ReportingStart != nil && report.Spec.ReportingEnd != nil && (report.Spec.ReportingStart.Time.After(report.Spec.ReportingEnd.Time) || report.Spec.ReportingStart.Time.Equal(report.Spec.ReportingEnd.Time)) {
		return op.setScheduledReportStatusValidationFailure(report, fmt.Sprintf("spec.reportingEnd (%s) must be after spec.reportingStart (%s)", report.Spec.ReportingEnd.Time, report.Spec.ReportingStart.Time))
	}
	if report.Spec.ReportingEnd == nil && report.Spec.RunImmediately {
		return op.setScheduledReportStatusValidationFailure(report, "spec.reportingEnd must be set if report.spec.runImmediately is true")
	}

	genQuery, err := op.reportGenerationQueryLister.ReportGenerationQueries(report.Namespace).Get(report.Spec.GenerationQueryName)
	if err != nil {
		logger.WithError(err).Errorf("failed to get report generation query")
		return err
	}

	queryDependencies, err := reporting.GetAndValidateGenerationQueryDependencies(
		reporting.NewReportGenerationQueryListerGetter(op.reportGenerationQueryLister),
		reporting.NewReportDataSourceListerGetter(op.reportDataSourceLister),
		reporting.NewScheduledReportListerGetter(op.scheduledReportLister),
		genQuery,
		op.uninitialiedDependendenciesHandler(),
	)
	if err != nil {
		return op.setScheduledReportStatusValidationFailure(report, fmt.Sprintf("failed to validate ReportGenerationQuery dependencies %s: %v", genQuery.Name, err))
	}

	// if it was previously failed validation, remove the status
	cbutil.RemoveScheduledReportCondition(&report.Status, cbTypes.ScheduledReportFailure)

	now := op.clock.Now().UTC()

	// set the lastReportTime if we've never collected before.
	if report.Status.LastReportTime == nil {
		if report.Spec.ReportingStart != nil {
			logger.Infof("no last report time for report, setting lastReportTime to spec.reportingStart %s", report.Spec.ReportingStart.Time)
			report.Status.LastReportTime = report.Spec.ReportingStart
		} else {
			logger.Infof("no last report time for report, setting lastReportTime to current time %s", now)
			// we try to align to the nearest minute
			nearestMinute := now.Truncate(time.Minute)
			report.Status.LastReportTime = &metav1.Time{nearestMinute}
		}
	} else {
		logger.Infof("last report time was %s", report.Status.LastReportTime.Time)
	}

	lastReportTime := report.Status.LastReportTime.Time

	var reportPeriod *reportPeriod
	if report.Spec.Schedule != nil {
		reportSchedule, err := getSchedule(report.Spec.Schedule)
		if err != nil {
			return err
		}

		reportPeriod = getNextReportPeriod(reportSchedule, report.Spec.Schedule.Period, lastReportTime)
	} else {
		reportPeriod, err = getRunOnceReportPeriod(report)
		if err != nil {
			return err
		}
	}

	if reportPeriod.periodStart.After(reportPeriod.periodEnd) {
		panic("periodStart should never come after periodEnd")
	}

	if report.Spec.ReportingEnd != nil && reportPeriod.periodEnd.After(report.Spec.ReportingEnd.Time) {
		logger.Debugf("calculated ScheduledReport periodEnd %s goes beyond spec.reportingEnd %s, setting periodEnd to reportingEnd", reportPeriod.periodEnd, report.Spec.ReportingEnd.Time)
		// we need to truncate the reportPeriod to align with the reportingEnd
		reportPeriod.periodEnd = report.Spec.ReportingEnd.Time
	}

	logger = logger.WithFields(log.Fields{
		"lastReportTime":    lastReportTime,
		"periodStart":       reportPeriod.periodStart,
		"periodEnd":         reportPeriod.periodEnd,
		"overwriteExisting": report.Spec.OverwriteExistingData,
	})

	var runningMsg string
	if report.Spec.RunImmediately {
		runningMsg = "ScheduledReport configured to run immediately"
		logger.Infof(runningMsg)
	} else {
		var gracePeriod time.Duration
		if report.Spec.GracePeriod != nil {
			gracePeriod = report.Spec.GracePeriod.Duration
		} else {
			gracePeriod = op.getDefaultReportGracePeriod()
			logger.Debugf("ScheduledReport has no gracePeriod configured, falling back to defaultGracePeriod: %s", gracePeriod)
		}

		nextRunTime := reportPeriod.periodEnd.Add(gracePeriod)
		reportGracePeriodUnmet := nextRunTime.After(now)
		waitTime := nextRunTime.Sub(now)

		if reportGracePeriodUnmet {
			// early check to see if an early reconcile occurred and if we're still
			// just waiting for the next reporting period, in which case, we can
			// just wait until the report period
			if runningCond != nil && runningCond.Reason == cbutil.ReportPeriodWaitingReason {
				logger.Debugf("ScheduledReport has a '%s' status with reason: '%s'. next scheduled report period is [%s to %s] with gracePeriod: %s. next run time is %s, waiting %s", cbTypes.ScheduledReportRunning, runningCond.Reason, reportPeriod.periodStart, reportPeriod.periodEnd, gracePeriod, nextRunTime, waitTime)
				op.enqueueScheduledReportAfter(report, waitTime)
				return nil
			}

			waitMsg := fmt.Sprintf("next scheduled report period is [%s to %s] with gracePeriod: %s. next run time is %s", reportPeriod.periodStart, reportPeriod.periodEnd, gracePeriod, nextRunTime)
			logger.Infof(waitMsg+". waiting %s", waitTime)

			report, err = op.updateScheduledReportStatusRunning(report, cbutil.ReportPeriodWaitingReason, waitMsg)
			if err != nil {
				return err
			}

			// we requeue this for later when the period we need to report on next
			// has elapsed
			op.enqueueScheduledReportAfter(report, waitTime)
			return nil
		} else {
			runningMsg = fmt.Sprintf("reached end of last reporting period [%s to %s]", reportPeriod.periodStart, reportPeriod.periodEnd)
			logger.Infof(runningMsg + ", running now")
		}
	}
	report, err = op.updateScheduledReportStatusRunning(report, cbutil.ScheduledReason, runningMsg)
	if err != nil {
		return err
	}

	tableName := reportingutil.ScheduledReportTableName(report.Name)
	// if tableName isn't set, this report is still new and we should make sure
	// no tables exist already in case of a previously failed cleanup.
	if report.Status.TableName == "" {
		logger.Debugf("dropping table %s", tableName)
		err = op.tableManager.DropTable(tableName, true)
		if err != nil {
			return fmt.Errorf("unable to drop table %s before creating for ScheduledReport %s: %v", tableName, report.Name, err)
		}

		columns := reportingutil.GenerateHiveColumns(genQuery)
		err = op.createTableForStorage(logger, report, cbTypes.SchemeGroupVersion.WithKind("ScheduledReport"), report.Spec.Output, tableName, columns, nil)
		if err != nil {
			logger.WithError(err).Error("error creating report table for scheduledReport")
			return err
		}

		report.Status.TableName = tableName
		report, err = op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
		if err != nil {
			logger.WithError(err).Errorf("unable to update ScheduledReport status with tableName")
			return err
		}
	}

	metricLabels := prometheus.Labels{
		"scheduledreport":       report.Name,
		"reportgenerationquery": report.Spec.GenerationQueryName,
		"table_name":            tableName,
	}

	genReportTotalCounter := generateScheduledReportTotalCounter.With(metricLabels)
	genReportFailedCounter := generateScheduledReportFailedCounter.With(metricLabels)
	genReportDurationObserver := generateScheduledReportDurationHistogram.With(metricLabels)

	genReportTotalCounter.Inc()
	generateReportStart := op.clock.Now()
	err = op.reportGenerator.GenerateReport(
		tableName,
		&reportPeriod.periodStart,
		&reportPeriod.periodEnd,
		genQuery,
		queryDependencies.DynamicReportGenerationQueries,
		report.Spec.Inputs,
		report.Spec.OverwriteExistingData,
	)
	generateReportDuration := op.clock.Since(generateReportStart)
	genReportDurationObserver.Observe(float64(generateReportDuration.Seconds()))

	if err != nil {
		genReportFailedCounter.Inc()
		// update the status to Failed with message containing the
		// error
		errMsg := fmt.Sprintf("error occurred while generating report: %s", err)
		_, updateErr := op.updateScheduledReportStatusFailure(report, cbutil.GenerateReportErrorReason, errMsg)
		if updateErr != nil {
			logger.WithError(updateErr).Errorf("unable to update ScheduledReport status")
			return updateErr
		}
		return fmt.Errorf("failed to generateReport for ScheduledReport %s, err: %v", report.Name, err)
	}
	// We generated a report successfully, remove any existing failure
	// conditions that may exist
	cbutil.RemoveScheduledReportCondition(&report.Status, cbTypes.ScheduledReportFailure)

	// Update the LastReportTime
	report.Status.LastReportTime = &metav1.Time{Time: reportPeriod.periodEnd}

	// check if we've reached the configured ReportingEnd, and if so, update
	// the status to indicate the report has finished
	finalRun := report.Spec.ReportingEnd != nil && report.Status.LastReportTime.Time.Equal(report.Spec.ReportingEnd.Time)
	if finalRun {
		// update the status to indicate the report doesn't need to run again
		msg := fmt.Sprintf("ScheduledReport has finished reporting. Report has reached the configured spec.reportingEnd: %s", report.Spec.ReportingEnd.Time)
		runningCond := cbutil.NewScheduledReportCondition(cbTypes.ScheduledReportRunning, v1.ConditionFalse, cbutil.ReportPeriodFinishedReason, msg)
		cbutil.SetScheduledReportCondition(&report.Status, *runningCond)
		logger.Infof(msg)
	}

	// update the report
	report, err = op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
	if err != nil {
		logger.WithError(err).Errorf("unable to update ScheduledReport status")
		return err
	}

	if err := op.queueDependentReportGenerationQueriesForScheduledReport(report); err != nil {
		logger.WithError(err).Errorf("error queuing ReportGenerationQuery dependents of ScheduledReport %s", report.Name)
	}

	if finalRun {
		return nil
	}

	// determine how long we have to wait until we should re run this handler,
	// if it's not a run-once report and then queue the report for that time
	if report.Spec.Schedule != nil {
		reportSchedule, err := getSchedule(report.Spec.Schedule)
		if err != nil {
			return err
		}

		reportPeriod = getNextReportPeriod(reportSchedule, report.Spec.Schedule.Period, report.Status.LastReportTime.Time)

		var gracePeriod time.Duration
		if report.Spec.GracePeriod != nil {
			gracePeriod = report.Spec.GracePeriod.Duration
		} else {
			gracePeriod = op.getDefaultReportGracePeriod()
		}

		now = op.clock.Now().UTC()
		nextRunTime := reportPeriod.periodEnd.Add(gracePeriod)
		waitTime := nextRunTime.Sub(now)
		op.enqueueScheduledReportAfter(report, waitTime)
	}
	return nil
}

func getRunOnceReportPeriod(report *cbTypes.ScheduledReport) (*reportPeriod, error) {
	if report.Spec.ReportingEnd == nil || report.Spec.ReportingStart == nil {
		return nil, fmt.Errorf("run-once reports must have both ReportingEnd and ReportingStart")
	}
	reportPeriod := &reportPeriod{
		periodStart: report.Spec.ReportingStart.UTC(),
		periodEnd:   report.Spec.ReportingEnd.UTC(),
	}
	return reportPeriod, nil
}

func getNextReportPeriod(schedule reportSchedule, period cbTypes.ScheduledReportPeriod, lastScheduled time.Time) *reportPeriod {
	periodStart := lastScheduled
	periodEnd := schedule.Next(periodStart)
	return &reportPeriod{
		periodEnd:   periodEnd.Truncate(time.Millisecond).UTC(),
		periodStart: periodStart.Truncate(time.Millisecond).UTC(),
	}
}

func convertDayOfWeek(dow string) (int, error) {
	switch strings.ToLower(dow) {
	case "sun", "sunday":
		return 0, nil
	case "mon", "monday":
		return 1, nil
	case "tue", "tues", "tuesday":
		return 2, nil
	case "wed", "weds", "wednesday":
		return 3, nil
	case "thur", "thurs", "thursday":
		return 4, nil
	case "fri", "friday":
		return 5, nil
	case "sat", "saturday":
		return 6, nil
	}
	return 0, fmt.Errorf("invalid day of week: %s", dow)
}

func (op *Reporting) addScheduledReportFinalizer(report *cbTypes.ScheduledReport) (*cbTypes.ScheduledReport, error) {
	report.Finalizers = append(report.Finalizers, scheduledReportFinalizer)
	newScheduledReport, err := op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
	logger := op.logger.WithField("ScheduledReport", report.Name)
	if err != nil {
		logger.WithError(err).Errorf("error adding %s finalizer to ScheduledReport: %s/%s", scheduledReportFinalizer, report.Namespace, report.Name)
		return nil, err
	}
	logger.Infof("added %s finalizer to ScheduledReport: %s/%s", scheduledReportFinalizer, report.Namespace, report.Name)
	return newScheduledReport, nil
}

func (op *Reporting) removeScheduledReportFinalizer(report *cbTypes.ScheduledReport) (*cbTypes.ScheduledReport, error) {
	if !slice.ContainsString(report.ObjectMeta.Finalizers, scheduledReportFinalizer, nil) {
		return report, nil
	}
	report.Finalizers = slice.RemoveString(report.Finalizers, scheduledReportFinalizer, nil)
	newScheduledReport, err := op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
	logger := op.logger.WithField("ScheduledReport", report.Name)
	if err != nil {
		logger.WithError(err).Errorf("error removing %s finalizer from ScheduledReport: %s/%s", scheduledReportFinalizer, report.Namespace, report.Name)
		return nil, err
	}
	logger.Infof("removed %s finalizer from ScheduledReport: %s/%s", scheduledReportFinalizer, report.Namespace, report.Name)
	return newScheduledReport, nil
}

func scheduledReportNeedsFinalizer(report *cbTypes.ScheduledReport) bool {
	return report.ObjectMeta.DeletionTimestamp == nil && !slice.ContainsString(report.ObjectMeta.Finalizers, scheduledReportFinalizer, nil)
}

// queueDependentReportGenerationQueriesForScheduledReport will queue all
// ReportGenerationQueries in the namespace which have a dependency on the
// scheduledReport
func (op *Reporting) queueDependentReportGenerationQueriesForScheduledReport(scheduledReport *cbTypes.ScheduledReport) error {
	queryLister := op.meteringClient.MeteringV1alpha1().ReportGenerationQueries(scheduledReport.Namespace)
	queries, err := queryLister.List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, query := range queries.Items {
		// look at the list Report of dependencies
		for _, dependency := range query.Spec.ScheduledReports {
			if dependency == scheduledReport.Name {
				// this query depends on the Report passed in
				op.enqueueReportGenerationQuery(query)
				break
			}
		}
	}
	return nil
}

func (op *Reporting) updateScheduledReportStatusFailure(report *cbTypes.ScheduledReport, reason, message string) (*cbTypes.ScheduledReport, error) {
	failureCondition := cbutil.NewScheduledReportCondition(cbTypes.ScheduledReportFailure, v1.ConditionTrue, reason, message)
	cbutil.RemoveScheduledReportCondition(&report.Status, cbTypes.ScheduledReportRunning)
	cbutil.SetScheduledReportCondition(&report.Status, *failureCondition)
	return op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
}

func (op *Reporting) updateScheduledReportStatusRunning(report *cbTypes.ScheduledReport, reason, message string) (*cbTypes.ScheduledReport, error) {
	runningCondition := cbutil.NewScheduledReportCondition(cbTypes.ScheduledReportRunning, v1.ConditionTrue, reason, message)
	cbutil.RemoveScheduledReportCondition(&report.Status, cbTypes.ScheduledReportFailure)
	cbutil.SetScheduledReportCondition(&report.Status, *runningCondition)
	return op.meteringClient.MeteringV1alpha1().ScheduledReports(report.Namespace).Update(report)
}

func (op *Reporting) setScheduledReportStatusValidationFailure(report *cbTypes.ScheduledReport, msg string) error {
	logger := op.logger.WithField("ScheduledReport", report.Name)
	failureCond := cbutil.GetScheduledReportCondition(report.Status, cbTypes.ScheduledReportFailure)
	previouslyFailedValidation := failureCond != nil && failureCond.Status == v1.ConditionTrue && failureCond.Reason == cbutil.FailedValidationReason

	if previouslyFailedValidation && failureCond.Message == msg && cbutil.GetScheduledReportCondition(report.Status, cbTypes.ScheduledReportFailure) == nil {
		// don't update unless the validation error changes
		logger.Debugf("ScheduledReport %s failed validation last reconcile, skipping updating status", report.Name)
		return nil
	}

	cbutil.RemoveScheduledReportCondition(&report.Status, cbTypes.ScheduledReportRunning)
	logger.Errorf("ScheduledReport %s failed validation: %s", report.Name, msg)
	report, err := op.updateScheduledReportStatusFailure(report, cbutil.FailedValidationReason, msg)
	if err != nil {
		logger.WithError(err).Errorf("unable to update ScheduledReport status")
		return err
	}
	return nil
}
