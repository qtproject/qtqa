/****************************************************************************
**
** Copyright (C) 2016 The Qt Company Ltd.
** Contact: https://www.qt.io/licensing/
**
** This file is part of the repo tools module of the Qt Toolkit.
**
** $QT_BEGIN_LICENSE:GPL-EXCEPT$
** Commercial License Usage
** Licensees holding valid commercial Qt licenses may use this file in
** accordance with the commercial license agreement provided with the
** Software or, alternatively, in accordance with the terms contained in
** a written agreement between you and The Qt Company. For licensing terms
** and conditions see https://www.qt.io/terms-conditions. For further
** information use the contact form at https://www.qt.io/contact-us.
**
** GNU General Public License Usage
** Alternatively, this file may be used under the terms of the GNU
** General Public License version 3 as published by the Free Software
** Foundation with exceptions as appearing in the file LICENSE.GPL3-EXCEPT
** included in the packaging of this file. Please review the following
** information to ensure the GNU General Public License requirements will
** be met: https://www.gnu.org/licenses/gpl-3.0.html.
**
** $QT_END_LICENSE$
**
****************************************************************************/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// PendingUpdate describes that a module needs an updated dependencies.yaml and we are waiting for the change
// to succeed/fail
type PendingUpdate struct {
	Module   *Module
	ChangeID string
}

// ModuleUpdateBatch is used to serialize and de-serialize the module updating state, used for debugging.
type ModuleUpdateBatch struct {
	Product           string
	Branch            string
	Todo              map[string]*Module
	Done              map[string]*Module
	Pending           []*PendingUpdate
	FailedModuleCount int
}

func (batch *ModuleUpdateBatch) scheduleUpdates(pushUserName string, manualStage bool) error {
	for _, moduleToUpdate := range batch.Todo {
		update, err := moduleToUpdate.updateDependenciesForModule(batch.Done)
		if err != nil {
			return fmt.Errorf("fatal error proposing module update: %s", err)
		}
		log.Printf("Attempting update for module %s resulted in %v\n", moduleToUpdate.RepoPath, update.result)
		if update.result == DependenciesUpdateContentUpToDate {
			batch.Done[moduleToUpdate.RepoPath] = moduleToUpdate
			delete(batch.Todo, moduleToUpdate.RepoPath)
		} else if update.result == DependenciesUpdateDependencyMissing {
			// Nothing to be done, we are waiting for indirect dependencies
		} else if update.result == DependenciesUpdateUpdateScheduled {
			// push and stage
			if err = pushChange(moduleToUpdate.RepoPath, moduleToUpdate.Branch, update.commitID, update.summary, pushUserName); err != nil {
				return fmt.Errorf("error pushing change upate: %s", err)
			}

			if !manualStage {
				if err = reviewAndStageChange(moduleToUpdate.RepoPath, moduleToUpdate.Branch, update.commitID, update.summary); err != nil {
					return fmt.Errorf("error pushing change upate: %s", err)
				}
			}

			batch.Pending = append(batch.Pending, &PendingUpdate{moduleToUpdate, update.changeID})
			delete(batch.Todo, moduleToUpdate.RepoPath)
		} else {
			return fmt.Errorf("invalid state returned by updateDependenciesForModule for %s", moduleToUpdate.RepoPath)
		}
	}

	return nil
}

func removeAllDirectAndIndirectDependencies(allModules *map[string]*Module, moduleToRemove string) {
	for moduleName, module := range *allModules {
		if module.hasDependency(moduleToRemove) {
			delete(*allModules, moduleName)
			removeAllDirectAndIndirectDependencies(allModules, module.RepoPath)
		}
	}
}

func (batch *ModuleUpdateBatch) checkPendingModules() {
	log.Println("Checking status of pending modules")
	var newPending []*PendingUpdate
	for _, pendingUpdate := range batch.Pending {
		module := pendingUpdate.Module
		status, err := getGerritChangeStatus(module.RepoPath, module.Branch, pendingUpdate.ChangeID)
		if err != nil {
			log.Printf("    status check of %s gave error: %s\n", module.RepoPath, err)
		} else {
			log.Printf("    status of %s: %s\n", module.RepoPath, status)
		}
		if err != nil || status == "STAGED" || status == "INTEGRATING" || status == "STAGING" {
			// no change yet
			newPending = append(newPending, pendingUpdate)
			continue
		} else if status == "MERGED" {
			module.refreshTip()
			batch.Done[module.RepoPath] = module
		} else {
			// Open or abandoned, not sure -- either way an error integrating the update
			removeAllDirectAndIndirectDependencies(&batch.Todo, module.RepoPath)
			batch.FailedModuleCount++
		}
	}
	batch.Pending = newPending
}

func loadTodoAndDoneModuleMapFromSubModules(branch string, submodules map[string]*submodule) (todo map[string]*Module, done map[string]*Module, err error) {
	todoModules := make(map[string]*Module)
	doneModules := make(map[string]*Module)

	for name, submodule := range submodules {
		// Erase modules that don't follow the qt5 branching scheme and don't need
		// dependencies.yaml
		if submodule.branch == "master" {
			continue
		}

		module, err := NewModule(name, branch, submodules)
		if err != nil {
			return nil, nil, fmt.Errorf("could not create internal module structure: %s", err)
		}

		if submodule.repoType == "inherited" || name == "qt/qtbase" {
			doneModules[module.RepoPath] = module
		} else {
			todoModules[module.RepoPath] = module
		}
	}

	return todoModules, doneModules, nil
}

func (batch *ModuleUpdateBatch) loadTodoList(qt5FetchRef string) error {
	qt5Modules, err := getQt5ProductModules(batch.Product, batch.Branch, qt5FetchRef)
	if err != nil {
		return fmt.Errorf("Error listing qt5 product modules: %s", err)
	}

	batch.Todo, batch.Done, err = loadTodoAndDoneModuleMapFromSubModules(batch.Branch, qt5Modules)
	return err
}

func sanitizeBranchOrRepo(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func (batch *ModuleUpdateBatch) stateFileName() string {
	return fmt.Sprintf("state_%s_%s.json", sanitizeBranchOrRepo(batch.Product), sanitizeBranchOrRepo(batch.Branch))
}

func (batch *ModuleUpdateBatch) saveState() error {
	fileName := batch.stateFileName()
	outputFile, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("failed to create state file %s: %s", fileName, err)
	}
	defer outputFile.Close()

	encoder := json.NewEncoder(outputFile)
	encoder.SetIndent("", "    ")
	return encoder.Encode(batch)
}

func (batch *ModuleUpdateBatch) loadState() error {
	fileName := batch.stateFileName()
	inputFile, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer inputFile.Close()

	decoder := json.NewDecoder(inputFile)
	err = decoder.Decode(batch)
	if err != nil {
		return fmt.Errorf("Error decoding JSON state file: %s", err)
	}
	return nil
}

func (batch *ModuleUpdateBatch) clearState() {
	os.Remove(batch.stateFileName())
}

func (batch *ModuleUpdateBatch) isDone() bool {
	return len(batch.Todo) == 0 && len(batch.Pending) == 0
}

func (batch *ModuleUpdateBatch) printSummary() {
	fmt.Fprintf(os.Stdout, "Summary of git repository dependency update for target branch %s based off of %s\n", batch.Branch, batch.Product)

	if batch.isDone() {
		if batch.FailedModuleCount > 0 {
			fmt.Fprintf(os.Stdout, "    %v modules failed to be updated. Check Gerrit for the %s branch\n", batch.FailedModuleCount, batch.Branch)
		} else {
			fmt.Fprintf(os.Stdout, "    No updates are necessary for any modules - everything is up-to-date\n")
		}
		return
	}

	if len(batch.Done) > 0 {
		fmt.Fprintf(os.Stdout, "The following modules have been brought up-to-date:\n")

		for name := range batch.Done {
			fmt.Println("    " + name)
		}
	}

	if len(batch.Pending) > 0 {
		fmt.Fprintf(os.Stdout, "The following modules are current in-progress:\n")

		for _, pending := range batch.Pending {
			fmt.Println("    " + pending.Module.RepoPath)
		}
	}

	fmt.Fprintf(os.Stdout, "The following modules are outdated and are either waiting for one of their dependencies or are ready for an update:\n")
	for name := range batch.Todo {
		fmt.Println("    " + name)
	}

	fmt.Println()
	fmt.Println()
}