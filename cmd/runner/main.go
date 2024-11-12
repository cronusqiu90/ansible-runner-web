package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"time"

	"fmt"
	"os"

	"github.com/apenella/go-ansible/v2/pkg/execute"
	"github.com/apenella/go-ansible/v2/pkg/execute/stdoutcallback"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
)

func main() {
	var inventoryPath, playbookPath, resultPath string
	flag.StringVar(&inventoryPath, "i", "", "inventory path")
	flag.StringVar(&playbookPath, "p", "", "playbook yaml path")
	flag.StringVar(&resultPath, "o", "", "store result path")
	flag.Parse()

	var err error
	buff := new(bytes.Buffer)

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()

	ansiblePlaybookOptions := &playbook.AnsiblePlaybookOptions{
		Become:  false,
		Verbose: true,
		ExtraVars: map[string]interface{}{
			"ansible_ssh_private_key_file": "/root/.ssh/id_rsa",
			"ansible_user":                 "auser",
			"ansible_port":                 8513,
		},
		Inventory:     inventoryPath,
		SSHCommonArgs: "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null",
		User:          "auser",
	}

	cmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(playbookPath),
		playbook.WithPlaybookOptions(ansiblePlaybookOptions),
	)
	fmt.Println(cmd.String())

	exec := stdoutcallback.NewDebugStdoutCallbackExecute(
		execute.NewDefaultExecute(
			execute.WithCmd(cmd),
			execute.WithEnvVars(map[string]string{"ANSIBLE_STDOUT_CALLBACK": "json"}),
			execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
			execute.WithWrite(io.Writer(buff)),
		),
	)

	err = exec.Execute(ctx)
	if err != nil {
		log.Fatal(err)
	}

	body, err := io.ReadAll(io.Reader(buff))
	if err != nil {
		log.Fatal(err)
	}

	if err := os.WriteFile(resultPath, body, 0644); err != nil {
		log.Fatal(err)
	}

}
