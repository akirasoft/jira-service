package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/andygrunwald/go-jira"
	cloudevents "github.com/cloudevents/sdk-go"
	"github.com/kelseyhightower/envconfig"
	keptnutils "github.com/keptn/go-utils/pkg/utils"
)

type envConfig struct {
	// Port on which to listen for cloudevents
	Port int    `envconfig:"RCV_PORT" default:"8080"`
	Path string `envconfig:"RCV_PATH" default:"/"`
}

type jiraConfig struct {
	Hostname string
	Username string
	Token    string
	Project  string
}

// JiraConf declaring this in outer block so that I don't have to pedantically pass it around
var JiraConf jiraConfig

// EvaluationDoneEvent ...
type EvaluationDoneEvent struct {
	Githuborg          string `json:"githuborg"`
	Project            string `json:"project"`
	Teststrategy       string `json:"teststrategy"`
	Deploymentstrategy string `json:"deploymentstrategy"`
	Stage              string `json:"stage"`
	Service            string `json:"service"`
	Image              string `json:"image"`
	Tag                string `json:"tag"`
	Evaluationpassed   bool   `json:"evaluationpassed"`
	Evaluationdetails  struct {
		Options struct {
			TimeStart int `json:"timeStart"`
			TimeEnd   int `json:"timeEnd"`
		} `json:"options"`
		TotalScore int `json:"totalScore"`
		Objectives struct {
			Pass    int `json:"pass"`
			Warning int `json:"warning"`
		} `json:"objectives"`
		// Data coming back from Prometheus sources is not strongly typed
		// especially within indicatorResults
		IndicatorResults []struct {
			ID         string `json:"id"`
			Violations []struct {
				Value interface{} `json:"value"`
				// we need to  take the key as raw json and parse it later
				Key       json.RawMessage `json:"key"`
				Breach    string          `json:"breach"`
				Threshold interface{}     `json:"threshold"`
			} `json:"violations"`
			Score int `json:"score"`
		} `json:"indicatorResults"`
		Result string `json:"result"`
	} `json:"evaluationdetails"`
}

// PrometheusKey is a json object containing job and an instance, we will use instance as it is more verbose
type PrometheusKey struct {
	Instance string `json:"instance"`
	Job      string `json:"job"`
}

//keptnHandler : receives keptn events via http and sets UFO LEDs based on payload
func keptnHandler(ctx context.Context, event cloudevents.Event) error {
	var shkeptncontext string
	event.Context.ExtensionAs("shkeptncontext", &shkeptncontext)

	logger := keptnutils.NewLogger(shkeptncontext, event.Context.GetID(), "jira-service")

	data := &EvaluationDoneEvent{}
	if err := event.DataAs(data); err != nil {
		//TODO: replace with keptn logger
		logger.Error(fmt.Sprintf("Got Data Error: %s", err.Error()))
		return err
	}

	if JiraConf.Project == "" {
		JiraConf.Project = strings.ToUpper(data.Project)
	}

	if event.Type() != "sh.keptn.events.evaluation-done" {
		const errorMsg = "Received unexpected keptn event"
		//TODO: replace with keptn logger
		logger.Error(errorMsg)
		return errors.New(errorMsg)
	}

	if event.Type() == "sh.keptn.events.evaluation-done" {
		if data.Evaluationpassed != true {
			//TODO: replace with keptn logger
			//don't put token in logs:
			logger.Info(fmt.Sprintf("Using JiraConfig: Hostname:%s, Username:%s, Project:%s", JiraConf.Hostname, JiraConf.Username, JiraConf.Project))
			go postJIRAIssue(JiraConf.Hostname, logger, *data, shkeptncontext)
		}
	}
	return nil
}

func postJIRAIssue(jiraHostname string, logger *keptnutils.Logger, data EvaluationDoneEvent, shkeptncontext string) {
	var strViolationsValue string
	var strKey string
	var strValThreshold string
	var keyDT string
	var keyProm PrometheusKey
	var indicatorValues string
	url := "https://" + jiraHostname

	// iterating through the contents of IndicatorResults so they can be sent to JIRA
	for i := 0; i < len(data.Evaluationdetails.IndicatorResults); i++ {
		for v := 0; v < len(data.Evaluationdetails.IndicatorResults[i].Violations); v++ {

			valDouble, ok := data.Evaluationdetails.IndicatorResults[i].Violations[v].Value.(float64)
			if ok {
				strViolationsValue = fmt.Sprintf("%f", valDouble)
			}
			valBoolean, ok := data.Evaluationdetails.IndicatorResults[i].Violations[v].Value.(bool)
			if ok {
				strViolationsValue = fmt.Sprintf("%t", valBoolean)
			}
			valString, ok := data.Evaluationdetails.IndicatorResults[i].Violations[v].Value.(string)
			if ok {
				strViolationsValue = valString
			}
			// threshold might not exist and should be a float64, if it is a string this will say it isn't there...
			valThreshold, ok := data.Evaluationdetails.IndicatorResults[i].Violations[v].Threshold.(float64)
			if ok {
				strValThreshold = strconv.FormatFloat(valThreshold, 'f', -1, 64)
			} else {
				strValThreshold = "No Threshold in Pitometer response"
			}

			if err := json.Unmarshal(data.Evaluationdetails.IndicatorResults[i].Violations[v].Key, &keyDT); err == nil {
				strKey = keyDT
			}
			// Prometheus Key is an object containing job and an instance, we will use instance as it is more verbose

			if err := json.Unmarshal(data.Evaluationdetails.IndicatorResults[i].Violations[v].Key, &keyProm); err == nil {
				strKey = keyProm.Instance
			}
			indicatorValues = indicatorValues +
				"\nIndicator ID: " + data.Evaluationdetails.IndicatorResults[i].ID +
				"\nIndicator Key: " + strKey +
				"\nIndicator Value: " + strViolationsValue +
				"\nIndicator Threshold: " + strValThreshold +
				"\nIndicator Breach: " + data.Evaluationdetails.IndicatorResults[i].Violations[v].Breach
		}
	}

	jiraIssue := "Keptn test evaluation failed, build was not deployed" +
		"\nshkeptncontext: " + shkeptncontext +
		"\nFailed stage: " + data.Stage +
		"\nFailed service: " + data.Service +
		"\nGithuborg: " + data.Githuborg +
		"\nTotal score: " + strconv.Itoa(data.Evaluationdetails.TotalScore) +
		"\nPass Threshold: " + strconv.Itoa(data.Evaluationdetails.Objectives.Pass) +
		"\nWarning Threshold: " + strconv.Itoa(data.Evaluationdetails.Objectives.Warning) +
		indicatorValues +
		"\nOverall Result: " + data.Evaluationdetails.Result

	tp := jira.BasicAuthTransport{
		Username: JiraConf.Username,
		Password: JiraConf.Token,
	}

	jiraClient, err := jira.NewClient(tp.Client(), url)
	if err != nil {
		panic(err)
	}

	i := jira.Issue{
		Fields: &jira.IssueFields{
			Assignee: &jira.User{
				Name: "admin",
			},
			Reporter: &jira.User{
				Name: "admin",
			},
			Description: jiraIssue,
			Type: jira.IssueType{
				Name: "Bug",
			},
			Project: jira.Project{
				Key: JiraConf.Project,
			},
			Summary: "Keptn Test Evaluation Failed",
		},
	}
	issue, response, err := jiraClient.Issue.Create(&i)

	if err != nil {
		// all this stuff is necessary to get back the response from JIRA if there is an error
		bodyBytes, _ := ioutil.ReadAll(response.Response.Body)
		bodyString := string(bodyBytes)
		logger.Error(fmt.Sprintf("JIRA returned: %s\n", bodyString))
		panic(err)
	}

	// use keptn logger
	logger.Info(fmt.Sprintf("JIRA returned Key:%s, ID:%+v\n", issue.Key, issue.ID))

}

func main() {
	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Printf("[ERROR] Failed to process listener var: %s", err)
		os.Exit(1)
	}
	err := envconfig.Process("jira", &JiraConf)
	if err != nil {
		log.Printf("[ERROR] Failed to process listener var: %s", err)
		os.Exit(1)
	}
	if JiraConf.Hostname == "" {
		log.Print("[ERROR] JIRA hostname not defined")
		os.Exit(1)
	}
	if JiraConf.Username == "" {
		log.Print("[ERROR] JIRA username not defined")
		os.Exit(1)
	}
	if JiraConf.Token == "" {
		log.Print("[ERROR] JIRA token not defined")
		os.Exit(1)
	}
	os.Exit(_main(os.Args[1:], env))
}

func _main(args []string, env envConfig) int {
	ctx := context.Background()

	t, err := cloudevents.NewHTTPTransport(
		cloudevents.WithPort(env.Port),
		cloudevents.WithPath(env.Path),
	)
	if err != nil {
		log.Printf("failed to create transport, %v", err)
		return 1
	}
	c, err := cloudevents.NewClient(t)
	if err != nil {
		log.Printf("failed to create client, %v", err)
		return 1
	}

	log.Fatalf("failed to start receiver: %s", c.StartReceiver(ctx, keptnHandler))
	return 0
}
