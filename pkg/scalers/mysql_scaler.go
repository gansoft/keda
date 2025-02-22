package scalers

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

type mySQLScaler struct {
	metricType v2beta2.MetricTargetType
	metadata   *mySQLMetadata
	connection *sql.DB
}

type mySQLMetadata struct {
	connectionString string // Database connection string
	username         string
	password         string
	host             string
	port             string
	dbName           string
	query            string
	queryValue       int64
	metricName       string
}

var mySQLLog = logf.Log.WithName("mysql_scaler")

// NewMySQLScaler creates a new MySQL scaler
func NewMySQLScaler(config *ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %s", err)
	}

	meta, err := parseMySQLMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing MySQL metadata: %s", err)
	}

	conn, err := newMySQLConnection(meta)
	if err != nil {
		return nil, fmt.Errorf("error establishing MySQL connection: %s", err)
	}
	return &mySQLScaler{
		metricType: metricType,
		metadata:   meta,
		connection: conn,
	}, nil
}

func parseMySQLMetadata(config *ScalerConfig) (*mySQLMetadata, error) {
	meta := mySQLMetadata{}

	if val, ok := config.TriggerMetadata["query"]; ok {
		meta.query = val
	} else {
		return nil, fmt.Errorf("no query given")
	}

	if val, ok := config.TriggerMetadata["queryValue"]; ok {
		queryValue, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("queryValue parsing error %s", err.Error())
		}
		meta.queryValue = queryValue
	} else {
		return nil, fmt.Errorf("no queryValue given")
	}

	switch {
	case config.AuthParams["connectionString"] != "":
		meta.connectionString = config.AuthParams["connectionString"]
	case config.TriggerMetadata["connectionStringFromEnv"] != "":
		meta.connectionString = config.ResolvedEnv[config.TriggerMetadata["connectionStringFromEnv"]]
	default:
		meta.connectionString = ""
		var err error
		meta.host, err = GetFromAuthOrMeta(config, "host")
		if err != nil {
			return nil, err
		}

		meta.port, err = GetFromAuthOrMeta(config, "port")
		if err != nil {
			return nil, err
		}

		meta.username, err = GetFromAuthOrMeta(config, "username")
		if err != nil {
			return nil, err
		}

		meta.dbName, err = GetFromAuthOrMeta(config, "dbName")
		if err != nil {
			return nil, err
		}

		if config.AuthParams["password"] != "" {
			meta.password = config.AuthParams["password"]
		} else if config.TriggerMetadata["passwordFromEnv"] != "" {
			meta.password = config.ResolvedEnv[config.TriggerMetadata["passwordFromEnv"]]
		}

		if len(meta.password) == 0 {
			return nil, fmt.Errorf("no password given")
		}
	}

	if meta.connectionString != "" {
		meta.dbName = parseMySQLDbNameFromConnectionStr(meta.connectionString)
	}
	meta.metricName = GenerateMetricNameWithIndex(config.ScalerIndex, kedautil.NormalizeString(fmt.Sprintf("mysql-%s", meta.dbName)))

	return &meta, nil
}

// metadataToConnectionStr builds new MySQL connection string
func metadataToConnectionStr(meta *mySQLMetadata) string {
	var connStr string

	if meta.connectionString != "" {
		connStr = meta.connectionString
	} else {
		// Build connection str
		config := mysql.NewConfig()
		config.Addr = fmt.Sprintf("%s:%s", meta.host, meta.port)
		config.DBName = meta.dbName
		config.Passwd = meta.password
		config.User = meta.username
		config.Net = "tcp"
		connStr = config.FormatDSN()
	}
	return connStr
}

// newMySQLConnection creates MySQL db connection
func newMySQLConnection(meta *mySQLMetadata) (*sql.DB, error) {
	connStr := metadataToConnectionStr(meta)
	db, err := sql.Open("mysql", connStr)
	if err != nil {
		mySQLLog.Error(err, fmt.Sprintf("Found error when opening connection: %s", err))
		return nil, err
	}
	err = db.Ping()
	if err != nil {
		mySQLLog.Error(err, fmt.Sprintf("Found error when pinging database: %s", err))
		return nil, err
	}
	return db, nil
}

// parseMySQLDbNameFromConnectionStr returns dbname from connection string
// in it is not able to parse it, it returns "dbname" string
func parseMySQLDbNameFromConnectionStr(connectionString string) string {
	splitted := strings.Split(connectionString, "/")

	if size := len(splitted); size > 0 {
		return splitted[size-1]
	}
	return "dbname"
}

// Close disposes of MySQL connections
func (s *mySQLScaler) Close(context.Context) error {
	err := s.connection.Close()
	if err != nil {
		mySQLLog.Error(err, "Error closing MySQL connection")
		return err
	}
	return nil
}

// IsActive returns true if there are pending messages to be processed
func (s *mySQLScaler) IsActive(ctx context.Context) (bool, error) {
	messages, err := s.getQueryResult(ctx)
	if err != nil {
		mySQLLog.Error(err, fmt.Sprintf("Error inspecting MySQL: %s", err))
		return false, err
	}
	return messages > 0, nil
}

// getQueryResult returns result of the scaler query
func (s *mySQLScaler) getQueryResult(ctx context.Context) (int64, error) {
	var value int64
	err := s.connection.QueryRowContext(ctx, s.metadata.query).Scan(&value)
	if err != nil {
		mySQLLog.Error(err, fmt.Sprintf("Could not query MySQL database: %s", err))
		return 0, err
	}
	return value, nil
}

// GetMetricSpecForScaling returns the MetricSpec for the Horizontal Pod Autoscaler
func (s *mySQLScaler) GetMetricSpecForScaling(context.Context) []v2beta2.MetricSpec {
	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: s.metadata.metricName,
		},
		Target: GetMetricTarget(s.metricType, s.metadata.queryValue),
	}
	metricSpec := v2beta2.MetricSpec{
		External: externalMetric, Type: externalMetricType,
	}
	return []v2beta2.MetricSpec{metricSpec}
}

// GetMetrics returns value for a supported metric and an error if there is a problem getting the metric
func (s *mySQLScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	num, err := s.getQueryResult(ctx)
	if err != nil {
		return []external_metrics.ExternalMetricValue{}, fmt.Errorf("error inspecting MySQL: %s", err)
	}

	metric := external_metrics.ExternalMetricValue{
		MetricName: metricName,
		Value:      *resource.NewQuantity(num, resource.DecimalSI),
		Timestamp:  metav1.Now(),
	}

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}
