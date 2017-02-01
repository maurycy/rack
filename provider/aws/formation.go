package aws

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/convox/rack/api/structs"
	"github.com/convox/rack/manifest"
)

// FormationList lists the Formation
func (p *AWSProvider) FormationList(app string) (structs.Formation, error) {
	log := Logger.At("FormationList").Start()

	a, err := p.AppGet(app)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	if a.Release == "" {
		return structs.Formation{}, nil
	}

	release, err := p.ReleaseGet(a.Name, a.Release)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	manifest, err := manifest.Load([]byte(release.Manifest))
	if err != nil {
		return nil, fmt.Errorf("could not parse manifest for release: %s", release.Id)
	}

	formation := structs.Formation{}

	for _, s := range manifest.Services {
		pf, err := processFormation(a, s)
		if err != nil {
			return nil, err
		}

		formation = append(formation, *pf)
	}

	log.Success()

	return formation, nil
}

// FormationGet gets a Formation
func (p *AWSProvider) FormationGet(app, process string) (*structs.ProcessFormation, error) {
	a, err := p.AppGet(app)
	if err != nil {
		return nil, err
	}
	if a.Release == "" {
		return nil, fmt.Errorf("no release for app: %s", app)
	}

	release, err := p.ReleaseGet(a.Name, a.Release)
	if err != nil {
		return nil, err
	}

	manifest, err := manifest.Load([]byte(release.Manifest))
	if err != nil {
		return nil, fmt.Errorf("could not parse manifest for release: %s", release.Id)
	}

	if _, ok := manifest.Services[process]; !ok {
		return nil, fmt.Errorf("no such process: %s", process)
	}

	return processFormation(a, manifest.Services[process])
}

// FormationSave saves a Formation
func (p *AWSProvider) FormationSave(app string, pf *structs.ProcessFormation) error {
	a, err := p.AppGet(app)
	if err != nil {
		return err
	}

	capacity, err := p.CapacityGet()
	if err != nil {
		return err
	}

	params := map[string]string{}

	if pf.Count < -1 {
		return fmt.Errorf("requested count %d must be -1 or greater", pf.Count)
	}

	if int64(pf.CPU) > capacity.InstanceCPU {
		return fmt.Errorf("requested cpu %d greater than instance size %d", pf.CPU, capacity.InstanceCPU)
	}

	if pf.CPU < 0 {
		return fmt.Errorf("requested cpu %d must be 0 or greater", pf.CPU)
	}

	if int64(pf.Memory) > capacity.InstanceMemory {
		return fmt.Errorf("requested memory %d greater than instance size %d", pf.Memory, capacity.InstanceMemory)
	}

	if _, ok := a.Parameters[fmt.Sprintf("%sFormation", upperName(pf.Name))]; ok {
		params[fmt.Sprintf("%sFormation", upperName(pf.Name))] = fmt.Sprintf("%d,%d,%d", pf.Count, pf.CPU, pf.Memory)
	} else {
		params[fmt.Sprintf("%sDesiredCount", upperName(pf.Name))] = fmt.Sprintf("%d", pf.Count)
		params[fmt.Sprintf("%sCpu", upperName(pf.Name))] = fmt.Sprintf("%d", pf.CPU)
		params[fmt.Sprintf("%sMemory", upperName(pf.Name))] = fmt.Sprintf("%d", pf.Memory)
	}

	p.EventSend(&structs.Event{
		Action: "release:scale",
		Data: map[string]interface{}{
			"app": a.Name,
			"id":  a.Release,
		},
	}, nil)

	err = p.updateStack(stackName(a), "", params)

	if ae, ok := err.(awserr.Error); ok {
		if ae.Code() == "ValidationError" {
			switch {
			case strings.Contains(ae.Error(), "No updates are to be performed"):
				return fmt.Errorf("no updates are to be performed: %s", app)
			case strings.Contains(ae.Error(), "can not be updated"):
				return fmt.Errorf("app is already updating: %s", app)
			}
		}
	}

	return err
}

func parseFormationParameters(app *structs.App, process string) (count, cpu, memory int, err error) {
	if _, ok := app.Parameters[fmt.Sprintf("%sFormation", upperName(process))]; ok {
		return parseFormationCombined(app, process)
	}

	return parseFormationIndividual(app, process)
}

func parseFormationCombined(app *structs.App, process string) (count, cpu, memory int, err error) {
	parts := strings.SplitN(app.Parameters[fmt.Sprintf("%sFormation", upperName(process))], ",", 3)

	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("%s formation settings not in Count,Cpu,Memory format", process)
	}

	count, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s count not numeric", process)
	}

	cpu, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s cpu not numeric", process)
	}

	memory, err = strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s memory not numeric", process)
	}

	return
}

func parseFormationIndividual(app *structs.App, process string) (count, cpu, memory int, err error) {
	count, err = strconv.Atoi(app.Parameters[fmt.Sprintf("%sDesiredCount", upperName(process))])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s count not numeric", process)
	}

	// backwards compatibility: old stacks that do not have a WebCpu Parameter should return 0, not an error
	if c, ok := app.Parameters[fmt.Sprintf("%sCpu", upperName(process))]; ok {
		cpu, err = strconv.Atoi(c)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%s cpu not numeric", process)
		}
	}

	memory, err = strconv.Atoi(app.Parameters[fmt.Sprintf("%sMemory", upperName(process))])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s memory not numeric", process)
	}

	return
}

func processFormation(a *structs.App, s manifest.Service) (*structs.ProcessFormation, error) {
	count, cpu, memory, err := parseFormationParameters(a, s.Name)
	if err != nil {
		return nil, err
	}

	re := regexp.MustCompile(fmt.Sprintf(`%sPort(\d+)Host`, upperName(s.Name)))

	ports := []int{}

	for key := range a.Parameters {
		matches := re.FindStringSubmatch(key)

		if len(matches) == 2 {
			port, _ := strconv.Atoi(matches[1])
			ports = append(ports, port)
		}
	}

	pf := &structs.ProcessFormation{
		Balancer: coalesceString(a.Outputs[fmt.Sprintf("Balancer%sHost", upperName(s.Name))], a.Outputs["BalancerHost"]),
		Name:     s.Name,
		Count:    count,
		Memory:   memory,
		CPU:      cpu,
		Ports:    ports,
	}

	return pf, nil
}
