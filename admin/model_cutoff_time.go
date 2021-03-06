/*
 * Paygate Admin API
 *
 * Paygate is a RESTful API enabling Automated Clearing House ([ACH](https://en.wikipedia.org/wiki/Automated_Clearing_House)) transactions to be submitted and received without a deep understanding of a full NACHA file specification.
 *
 * API version: v1
 * Generated by: OpenAPI Generator (https://openapi-generator.tech)
 */

package admin

// CutoffTime struct for CutoffTime
type CutoffTime struct {
	// 24-hour timestamp for last processing minute
	Cutoff float32 `json:"cutoff"`
	// IANA timezone name for cutoff time
	Location string `json:"location"`
}
