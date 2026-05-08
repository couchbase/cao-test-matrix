pipeline {
  agent {
    label 'k8'
  }

  triggers {
    // Every Friday at 9:00am UTC
    cron('0 9 * * 5')
  }

  options {
    timestamps()

    buildDiscarder(
      logRotator(
        artifactDaysToKeepStr: '30',
        artifactNumToKeepStr: '30',
        daysToKeepStr: '30',
        numToKeepStr: '30',
      )
    )
  }

  parameters {
    string(name: 'config_path', defaultValue: 'matrix-config.json', description: 'Path to matrix config JSON')
    string(name: 'date_override', defaultValue: '', description: 'Override date for testing (YYYY-MM-DD), leave empty for today')
    booleanParam(name: 'dry_run', defaultValue: false, description: 'Print parameters without triggering sub-jobs')
  }

  environment {
    GOVERSION = '1.22.1'
    GOROOT = "${env.WORKSPACE}/go"
    PATH = "${env.WORKSPACE}/go/bin:${env.WORKSPACE}/bin:$PATH"
  }

  stages {
    stage('Prerequisites') {
      steps {
        sh "mkdir -p ${env.WORKSPACE}/bin"
        sh "wget -q -O- https://dl.google.com/go/go${env.GOVERSION}.linux-amd64.tar.gz | tar xz"
      }
    }

    stage('Discover Branches') {
      steps {
        script {
          def branchesJson = sh(
            script: "go run generate-matrix.go -config ${params.config_path} -list-branches",
            returnStdout: true
          ).trim()

          env.ENABLED_BRANCHES = branchesJson
          echo "Enabled branches: ${branchesJson}"
        }
      }
    }

    stage('Generate & Dispatch') {
      steps {
        script {
          def branches = new groovy.json.JsonSlurper().parseText(env.ENABLED_BRANCHES)
          def dateArg = ''
          if (params.date_override?.trim()) {
            dateArg = "-date ${params.date_override}"
          }

          def descriptions = []

          for (branchName in branches) {
            echo "=== Generating matrix for branch: ${branchName} ==="

            def rawOutput = ''
            withCredentials([usernamePassword(credentialsId: 'couchbaseqe-ghrc', usernameVariable: 'GHCR_USER', passwordVariable: 'GHCR_PASS')]) {
              rawOutput = sh(
                script: 'go run generate-matrix.go -config ' + params.config_path + ' -branch ' + branchName + ' ' + dateArg + ' -ghcr-user $GHCR_USER -ghcr-pass $GHCR_PASS 2>generate-matrix-' + branchName + '.log',
                returnStdout: true
              ).trim()
            }

            sh "cat generate-matrix-${branchName}.log"

            def matrix = new groovy.json.JsonSlurper().parseText(rawOutput)
            echo "Generated matrix for ${branchName}:\n${rawOutput}"

            descriptions.add("${matrix.platform}/${branchName}/k8s:${matrix.kubernetes_version}/srv:${matrix.server_image}")

            if (params.dry_run) {
              echo "DRY RUN: would trigger ${matrix.platform} job for branch ${branchName}"
              continue
            }

            def commonParams = [
              string(name: 'refspec', value: matrix.refspec),
              string(name: 'operator_image', value: matrix.operator_image),
              string(name: 'admission_image', value: matrix.admission_image),
              string(name: 'certification_image', value: matrix.certification_image),
              string(name: 'server_image', value: matrix.server_image),
              string(name: 'server_image_upgrade', value: matrix.server_image_upgrade),
              string(name: 'backup_image', value: matrix.backup_image),
              string(name: 'exporter_image', value: matrix.exporter_image),
              string(name: 'exporter_image_upgrade', value: matrix.exporter_image_upgrade),
              string(name: 'logging_image', value: matrix.logging_image),
              string(name: 'logging_image_upgrade', value: matrix.logging_image_upgrade),
              string(name: 'cloud_native_gateway_image', value: matrix.cloud_native_gateway_image),
              string(name: 'mobile_image', value: matrix.mobile_image),
              string(name: 'storage_class', value: matrix.storage_class),
              booleanParam(name: 'validation', value: true),
              booleanParam(name: 'sanity', value: true),
              booleanParam(name: 'p0', value: true),
              booleanParam(name: 'p1', value: true),
              booleanParam(name: 'platform', value: true),
              booleanParam(name: 'first_rerun', value: true),
              booleanParam(name: 'second_rerun', value: true),
            ]

            switch (matrix.platform) {
              case 'GKE':
                build job: 'k8s-cbop-gke-pipeline', parameters: commonParams + [
                  string(name: 'kubernetes_version', value: matrix.kubernetes_version),
                  string(name: 'kubectl_version', value: matrix.kubectl_version),
                ], wait: false
                break

              case 'EKS':
                build job: 'k8s-cbop-eks-pipeline', parameters: commonParams + [
                  string(name: 'kubernetes_version', value: matrix.kubernetes_version),
                  string(name: 'kubectl_version', value: matrix.kubectl_version),
                ], wait: false
                break

              case 'AKS':
                build job: 'k8s-cbop-aks-pipeline', parameters: commonParams + [
                  string(name: 'kubernetes_version', value: matrix.kubernetes_version),
                  string(name: 'kubectl_version', value: matrix.kubectl_version),
                ], wait: false
                break

              case 'OpenShift':
                build job: 'k8s-cbop-oc-pipeline', parameters: commonParams + [
                  string(name: 'openshift_version', value: matrix.kubernetes_version),
                ], wait: false
                break

              default:
                echo "WARNING: Unknown platform ${matrix.platform}, skipping"
            }

            echo "Triggered ${matrix.platform} pipeline for branch ${branchName}"
          }

          currentBuild.description = descriptions.join('\n')
        }
      }
    }
  }

  post {
    always {
      archiveArtifacts artifacts: 'generate-matrix-*.log', allowEmptyArchive: true
      cleanWs()
    }
  }
}
