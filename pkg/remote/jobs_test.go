package remote

import (
	"testing"
)

func TestJobRequestValidation(t *testing.T) {
	for _, request := range []*Request{
		{Job: &JobRequest{Operation: JobStart}, Arguments: []string{"echo"}},
		{Job: &JobRequest{Operation: JobStart, Log: "out.log"}, Script: &Script{Name: "-"}},
		{Job: &JobRequest{Operation: JobList}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd"}},
		{Job: &JobRequest{Operation: JobKill, ID: "ffffffff"}},
	} {
		if err := request.EnsureValid(); err != nil {
			t.Errorf("valid job request %+v rejected: %v", request.Job, err)
		}
	}
	for _, request := range []*Request{
		{Job: &JobRequest{Operation: JobStart}},
		{Job: &JobRequest{Operation: JobStart, ID: "0123abcd"}, Arguments: []string{"echo"}},
		{Job: &JobRequest{Operation: JobStart, Log: "\x00"}, Arguments: []string{"echo"}},
		{Job: &JobRequest{Operation: JobList, ID: "0123abcd"}},
		{Job: &JobRequest{Operation: JobList}, Arguments: []string{"echo"}},
		{Job: &JobRequest{Operation: JobWait}},
		{Job: &JobRequest{Operation: JobWait, ID: "../x"}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123ABCD"}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abc"}},
		{Job: &JobRequest{Operation: JobKill, ID: "0123abcd", Log: "out.log"}},
		{Job: &JobRequest{Operation: JobKill, ID: "0123abcd"}, Script: &Script{Name: "-"}},
		{Job: &JobRequest{Operation: "other"}},
		{Job: &JobRequest{Operation: JobList}, File: "a"},
	} {
		if request.EnsureValid() == nil {
			t.Errorf("invalid job request %+v accepted", request.Job)
		}
	}
}
