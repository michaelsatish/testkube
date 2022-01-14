/*
 * TestKube API
 *
 * TestKube provides a Kubernetes-native framework for test definition, execution and results
 *
 * API version: 1.0.0
 * Contact: testkube@kubeshop.io
 * Generated by: Swagger Codegen (https://github.com/swagger-api/swagger-codegen.git)
 */
package testkube

import "fmt"

func (r TestExecutionsResult) Table() (headers []string, output [][]string) {
	headers = []string{"ID", "Test Name", "Execution Name", "Status", "Steps"}

	for _, result := range r.Results {
		output = append(output, []string{
			result.Id,
			result.TestName,
			result.Name,
			string(*result.Status),
			fmt.Sprintf("%d", len(result.Execution)),
		})
	}

	return
}
