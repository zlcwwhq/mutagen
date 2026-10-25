package remote

import (
	"testing"
	"time"
)

func TestJobRequestValidation(t *testing.T) {
	for _, request := range []*Request{
		{Job: &JobRequest{Operation: JobStart}, Arguments: []string{"echo"}},
		{Job: &JobRequest{Operation: JobStart, Log: "out.log"}, Script: &Script{Name: "-"}},
		{Job: &JobRequest{Operation: JobList}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd"}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Tail: 20}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Tail: 20, Timeout: time.Second}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Tail: true}, Timeout: time.Second}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Tail: true, Lines: 20}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Offset: 4096, Identity: "66306:1234"}}},
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
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Tail: -1}},
		{Job: &JobRequest{Operation: JobList, Tail: 1}},
		{Job: &JobRequest{Operation: JobKill, ID: "0123abcd", Tail: 1}},
		{Job: &JobRequest{Operation: JobStart, Tail: 1}, Arguments: []string{"true"}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Timeout: -1}},
		{Job: &JobRequest{Operation: JobKill, ID: "0123abcd", Timeout: time.Second}},
		{Job: &JobRequest{Operation: JobList, Timeout: time.Second}},
		{Job: &JobRequest{Operation: JobKill, ID: "0123abcd", Follow: &JobFollow{}}},
		{Job: &JobRequest{Operation: JobStart, Follow: &JobFollow{}}, Arguments: []string{"true"}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Tail: 1, Follow: &JobFollow{}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Tail: true, Lines: -1}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Offset: -1}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Lines: 1}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Tail: true, Offset: 1}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Tail: true, Identity: "1:2"}}},
		{Job: &JobRequest{Operation: JobWait, ID: "0123abcd", Follow: &JobFollow{Identity: "\x00"}}},
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
